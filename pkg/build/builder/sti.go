package builder

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	dockerclient "github.com/fsouza/go-dockerclient"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	buildapiv1 "github.com/openshift/api/build/v1"
	buildclientv1 "github.com/openshift/client-go/build/clientset/versioned/typed/build/v1"
	"github.com/openshift/imagebuilder"
	"github.com/openshift/library-go/pkg/git"
	s2iapi "github.com/openshift/source-to-image/pkg/api"
	s2iconstants "github.com/openshift/source-to-image/pkg/api/constants"
	"github.com/openshift/source-to-image/pkg/api/describe"
	"github.com/openshift/source-to-image/pkg/api/validation"
	s2ibuild "github.com/openshift/source-to-image/pkg/build"
	s2i "github.com/openshift/source-to-image/pkg/build/strategies"
	"github.com/openshift/source-to-image/pkg/docker"
	s2igit "github.com/openshift/source-to-image/pkg/scm/git"
	s2iutil "github.com/openshift/source-to-image/pkg/util"

	"github.com/openshift/builder/pkg/build/builder/cmd/dockercfg"
	"github.com/openshift/builder/pkg/build/builder/timing"
	builderutil "github.com/openshift/builder/pkg/build/builder/util"
	"github.com/openshift/builder/pkg/build/builder/util/dockerfile"

	"github.com/google/uuid"
)

// builderFactory is the internal interface to decouple S2I-specific code from Origin builder code
type builderFactory interface {
	// Create S2I Builder based on S2I configuration
	Builder(config *s2iapi.Config, overrides s2ibuild.Overrides) (s2ibuild.Builder, s2iapi.BuildInfo, error)
}

// validator is the interval interface to decouple S2I-specific code from Origin builder code
type validator interface {
	// Perform validation of S2I configuration, returns slice of validation errors
	ValidateConfig(config *s2iapi.Config) []validation.Error
}

// runtimeBuilderFactory is the default implementation of stiBuilderFactory
type runtimeBuilderFactory struct {
	dockerClient DockerClient
}

// Builder delegates execution to S2I-specific code
func (r runtimeBuilderFactory) Builder(config *s2iapi.Config, overrides s2ibuild.Overrides) (s2ibuild.Builder, s2iapi.BuildInfo, error) {
	var client docker.Client
	var err error
	builder, buildInfo, err := s2i.Strategy(client, config, overrides)
	return builder, buildInfo, err
}

// runtimeConfigValidator is the default implementation of stiConfigValidator
type runtimeConfigValidator struct{}

// ValidateConfig delegates execution to S2I-specific code
func (_ runtimeConfigValidator) ValidateConfig(config *s2iapi.Config) []validation.Error {
	return validation.ValidateConfig(config)
}

// S2IBuilder performs an STI build given the build object
type S2IBuilder struct {
	builder      builderFactory
	validator    validator
	dockerClient DockerClient
	dockerSocket string
	build        *buildapiv1.Build
	client       buildclientv1.BuildInterface
	cgLimits     *s2iapi.CGroupLimits
}

// NewS2IBuilder creates a new STIBuilder instance
func NewS2IBuilder(dockerClient DockerClient, dockerSocket string, buildsClient buildclientv1.BuildInterface, build *buildapiv1.Build,
	cgLimits *s2iapi.CGroupLimits) *S2IBuilder {
	// delegate to internal implementation passing default implementation of builderFactory and validator
	return newS2IBuilder(dockerClient, dockerSocket, buildsClient, build, runtimeBuilderFactory{dockerClient}, runtimeConfigValidator{}, cgLimits)
}

// newS2IBuilder is the internal factory function to create STIBuilder based on parameters. Used for testing.
func newS2IBuilder(dockerClient DockerClient, dockerSocket string, buildsClient buildclientv1.BuildInterface, build *buildapiv1.Build,
	builder builderFactory, validator validator, cgLimits *s2iapi.CGroupLimits) *S2IBuilder {
	// just create instance
	return &S2IBuilder{
		builder:      builder,
		validator:    validator,
		dockerClient: dockerClient,
		dockerSocket: dockerSocket,
		build:        build,
		client:       buildsClient,
		cgLimits:     cgLimits,
	}
}

// injectConfigMaps creates an s2i `VolumeSpec` from each provided `ConfigMapBuildSource`
func injectConfigMaps(configMaps []buildapiv1.ConfigMapBuildSource) []s2iapi.VolumeSpec {
	vols := make([]s2iapi.VolumeSpec, len(configMaps))
	for i, c := range configMaps {
		vols[i] = makeVolumeSpec(configMapSource(c), configMapBuildSourceBaseMountPath)
	}
	return vols
}

// injectSecrets creates an s2i `VolumeSpec` from each provided `SecretBuildSource`
func injectSecrets(secrets []buildapiv1.SecretBuildSource) []s2iapi.VolumeSpec {
	vols := make([]s2iapi.VolumeSpec, len(secrets))
	for i, s := range secrets {
		vols[i] = makeVolumeSpec(secretSource(s), secretBuildSourceBaseMountPath)
	}
	return vols
}

func makeVolumeSpec(src localObjectBuildSource, mountPath string) s2iapi.VolumeSpec {
	log.V(3).Infof("Injecting build source %q into a build into %q", src.LocalObjectRef().Name, filepath.Clean(src.DestinationPath()))
	return s2iapi.VolumeSpec{
		Source:      filepath.Join(mountPath, src.LocalObjectRef().Name),
		Destination: src.DestinationPath(),
		Keep:        !src.IsSecret(),
	}
}

// Build executes S2I build based on configured builder, S2I builder factory
// and S2I config validator
func (s *S2IBuilder) Build() error {
	var err error
	ctx := timing.NewContext(context.Background())
	defer func() {
		s.build.Status.Stages = timing.AppendStageAndStepInfo(s.build.Status.Stages, timing.GetStages(ctx))
		HandleBuildStatusUpdate(s.build, s.client, nil)
	}()

	if s.build.Spec.Strategy.SourceStrategy == nil {
		return errors.New("the source to image builder must be used with the source strategy")
	}

	var push bool
	// if there is no output target, set one up so the docker build logic
	// (which requires a tag) will still work, but we won't push it at the end.
	if s.build.Spec.Output.To == nil || len(s.build.Spec.Output.To.Name) == 0 {
		s.build.Status.OutputDockerImageReference = s.build.Name
	} else {
		push = true
	}
	pushTag := s.build.Status.OutputDockerImageReference

	sourceInfo, err := readSourceInfo()
	if err != nil {
		return fmt.Errorf("error reading git source info: %v", err)
	}
	var s2iSourceInfo *s2igit.SourceInfo
	if sourceInfo != nil {
		s2iSourceInfo = toS2ISourceInfo(sourceInfo)
	}
	injections := s2iapi.VolumeList{}
	injections = append(injections, injectSecrets(s.build.Spec.Source.Secrets)...)
	injections = append(injections, injectConfigMaps(s.build.Spec.Source.ConfigMaps)...)

	buildTag := randomBuildTag(s.build.Namespace, s.build.Name)
	scriptDownloadProxyConfig, err := scriptProxyConfig(s.build)
	if err != nil {
		return err
	}
	if scriptDownloadProxyConfig != nil {
		log.V(0).Infof("Using HTTP proxy %v and HTTPS proxy %v for script download",
			builderutil.SafeForLoggingURL(scriptDownloadProxyConfig.HTTPProxy),
			builderutil.SafeForLoggingURL(scriptDownloadProxyConfig.HTTPSProxy),
		)
	}

	var incremental bool
	if s.build.Spec.Strategy.SourceStrategy.Incremental != nil {
		incremental = *s.build.Spec.Strategy.SourceStrategy.Incremental
	}

	srcDir := InputContentPath
	contextDir := ""
	if len(s.build.Spec.Source.ContextDir) != 0 {
		contextDir = filepath.Clean(s.build.Spec.Source.ContextDir)
		if contextDir == "." || contextDir == "/" {
			contextDir = ""
		}
	}

	config := &s2iapi.Config{
		// Save some processing time by not cleaning up (the container will go away anyway)
		PreserveWorkingDir: true,
		WorkingDir:         "/tmp",
		DockerConfig:       &s2iapi.DockerConfig{Endpoint: s.dockerSocket},
		DockerCfgPath:      os.Getenv(dockercfg.PullAuthType),
		LabelNamespace:     builderutil.DefaultDockerLabelNamespace,

		ScriptsURL: s.build.Spec.Strategy.SourceStrategy.Scripts,

		BuilderImage:       s.build.Spec.Strategy.SourceStrategy.From.Name,
		BuilderPullPolicy:  s2iapi.PullAlways,
		Incremental:        incremental,
		IncrementalFromTag: pushTag,

		Environment: buildEnvVars(s.build, sourceInfo),
		Labels:      s2iBuildLabels(s.build, sourceInfo),

		Source:     &s2igit.URL{URL: url.URL{Path: srcDir}, Type: s2igit.URLTypeLocal},
		ContextDir: contextDir,
		SourceInfo: s2iSourceInfo,
		ForceCopy:  true,
		Injections: injections,

		AsDockerfile: "/tmp/dockercontext/Dockerfile",

		ScriptDownloadProxyConfig: scriptDownloadProxyConfig,
		BlockOnBuild:              true,

		KeepSymlinks: true,
	}

	// If DockerCfgPath is provided in buildapiv1.Config, then attempt to read the
	// dockercfg file and get the authentication for pulling the images.

	if s.build.Spec.Strategy.SourceStrategy.ForcePull || !isImagePresent(s.dockerClient, config.BuilderImage) {
		startTime := metav1.Now()
		searchPaths := dockercfg.NewHelper().GetDockerAuthSearchPaths(dockercfg.PullAuthType)
		err = s.pullImage(config.BuilderImage, searchPaths)
		timing.RecordNewStep(ctx, buildapiv1.StagePullImages, buildapiv1.StepPullBaseImage, startTime, metav1.Now())
		if err != nil {
			return err
		}
	}

	if config.Incremental {
		if s.build.Spec.Strategy.SourceStrategy.ForcePull || !isImagePresent(s.dockerClient, config.IncrementalFromTag) {
			// Per @bparees the dockercfg.PushTypeAuth is needed to use the same credentials/authentication that
			// we used to push the image previously.
			searchPaths := dockercfg.NewHelper().GetDockerAuthSearchPaths(dockercfg.PushAuthType)
			startTime := metav1.Now()
			err = s.pullImage(config.IncrementalFromTag, searchPaths)
			timing.RecordNewStep(ctx, buildapiv1.StagePullImages, buildapiv1.StepPullInputImage, startTime, metav1.Now())
			// If there was an error, the incremental image may not exist. Treat the build as a normal s2i build.
			if err != nil {
				log.V(2).Infof("Failed to pull incremental builder image %s - executing normal s2i build instead.", config.IncrementalFromTag)
				log.V(5).Infof("Incremental image pull failure: %v", err)
				config.Incremental = false
				config.IncrementalFromTag = ""
			} else {
				// with the 'special case pull' of the incremental image above, set force pull to false so that the
				// logic below does not attempt to re-pull the previous-output-image with the standard pull secret
				// (which would potentially fail since the image should be pulled w/ the push credentails)
				s.build.Spec.Strategy.SourceStrategy.ForcePull = false
			}
		}
	}

	assembleUser, err := getAssembleUser(s.dockerClient, config.BuilderImage)
	if err != nil {
		return err
	}
	if len(assembleUser) > 0 {
		log.V(4).Infof("Using builder image assemble user %s", assembleUser)
		config.AssembleUser = assembleUser
	}

	labels, err := getImageLabels(s.dockerClient, config.BuilderImage)
	if err != nil {
		return err
	}
	destination := labels[s2iconstants.DestinationLabel]
	if len(destination) > 0 {
		log.V(4).Infof("Using builder image destination %s", destination)
		config.Destination = destination
	}
	if len(config.ScriptsURL) == 0 {
		scriptsURL := labels[s2iconstants.ScriptsURLLabel]
		if len(scriptsURL) > 0 {
			log.V(4).Infof("Using builder scripts URL %s", destination)
			config.ImageScriptsURL = scriptsURL
		}
	}

	allowedUIDs := os.Getenv(builderutil.AllowedUIDs)
	log.V(4).Infof("The value of %s is [%s]", builderutil.AllowedUIDs, allowedUIDs)
	if len(allowedUIDs) > 0 {
		err = config.AllowedUIDs.Set(allowedUIDs)
		if err != nil {
			return err
		}
	}

	if errs := s.validator.ValidateConfig(config); len(errs) != 0 {
		var buffer bytes.Buffer
		for _, ve := range errs {
			buffer.WriteString(ve.Error())
			buffer.WriteString(", ")
		}
		return errors.New(buffer.String())
	}

	if log.Is(4) {
		redactedConfig := SafeForLoggingS2IConfig(config)
		log.V(4).Infof("Creating a new S2I builder with config: %#v\n", describe.Config(nil, redactedConfig))
	}
	builder, buildInfo, err := s.builder.Builder(config, s2ibuild.Overrides{Downloader: nil})
	if err != nil {
		s.build.Status.Phase = buildapiv1.BuildPhaseFailed
		s.build.Status.Reason, s.build.Status.Message = convertS2IFailureType(
			buildInfo.FailureReason.Reason,
			buildInfo.FailureReason.Message,
		)
		HandleBuildStatusUpdate(s.build, s.client, nil)
		return err
	}

	log.V(4).Infof("Starting S2I build from %s/%s BuildConfig ...", s.build.Namespace, s.build.Name)
	log.Infof("Generating dockerfile with builder image %s", s.build.Spec.Strategy.SourceStrategy.From.Name)
	result, err := builder.Build(config)

	for _, stage := range result.BuildInfo.Stages {
		for _, step := range stage.Steps {
			timing.RecordNewStep(ctx, buildapiv1.StageName(stage.Name), buildapiv1.StepName(step.Name), metav1.NewTime(step.StartTime), metav1.NewTime(step.StartTime.Add(time.Duration(step.DurationMilliseconds)*time.Millisecond)))
		}
	}

	if err != nil {
		s.build.Status.Phase = buildapiv1.BuildPhaseFailed
		if result != nil {
			s.build.Status.Reason, s.build.Status.Message = convertS2IFailureType(
				result.BuildInfo.FailureReason.Reason,
				result.BuildInfo.FailureReason.Message,
			)
		} else {
			s.build.Status.Reason = buildapiv1.StatusReasonGenericBuildFailed
			s.build.Status.Message = "Generic Build failure - check logs for details."
		}

		HandleBuildStatusUpdate(s.build, s.client, nil)
		return err
	}

	opts := dockerclient.BuildImageOptions{
		Context:             ctx,
		Name:                buildTag,
		RmTmpContainer:      true,
		ForceRmTmpContainer: true,
		OutputStream:        os.Stdout,
		Dockerfile:          defaultDockerfilePath,
		NoCache:             false,
		Pull:                s.build.Spec.Strategy.SourceStrategy.ForcePull,
		ContextDir:          "/tmp/dockercontext",
	}

	log.V(0).Infof("pushTag: %s", pushTag)
	log.V(0).Infof("buildTag: %s", buildTag)
	log.V(0).Infof("defaultDcokerfilePath: %s", defaultDockerfilePath)

	if s.cgLimits != nil {
		opts.CPUPeriod = s.cgLimits.CPUPeriod
		opts.CPUQuota = s.cgLimits.CPUQuota
		opts.CPUShares = s.cgLimits.CPUShares
		opts.Memory = s.cgLimits.MemoryLimitBytes
		opts.Memswap = s.cgLimits.MemorySwap
		opts.CgroupParent = s.cgLimits.Parent
	}

	pullAuthConfigs := s.setupPullSecret()
	if pullAuthConfigs != nil {
		opts.AuthConfigs = *pullAuthConfigs
	}

	startTime := metav1.Now()
	if _, err := os.Stat(config.AsDockerfile); !os.IsNotExist(err) {
		in, err := ioutil.ReadFile(config.AsDockerfile)
		if err != nil {
			return err
		}
		node, err := imagebuilder.ParseDockerfile(bytes.NewBuffer(in))
		if err != nil {
			return err
		}
		// Append post commit
		if err := appendPostCommit(node, buildPostCommit(s.build.Spec.PostCommit)); err != nil {
			return err
		}
		out := dockerfile.Write(node)
		log.V(4).Infof("Replacing dockerfile\n%s\nwith:\n%s", string(in), string(out))
		overwriteFile(config.AsDockerfile, out)
	}
	// TODO pass ImageOptimization policy to the build?
	err = s.dockerClient.BuildImage(opts)
	timing.RecordNewStep(ctx, buildapiv1.StageBuild, buildapiv1.StepDockerBuild, startTime, metav1.Now())
	if err != nil {
		// TODO: Create new error states
		s.build.Status.Phase = buildapiv1.BuildPhaseFailed
		s.build.Status.Reason = buildapiv1.StatusReasonGenericBuildFailed
		s.build.Status.Message = builderutil.StatusMessageGenericBuildFailed
		return err
	}

	var cw_env = ""
	var cw_workdir = ""
	var cw_password = ""

	if strings.Contains(buildTag, "-cw") {
		cmd := exec.Command("python3", "/usr/bin/extract_env.py", buildTag)
		stdout, err := cmd.Output()
		if err != nil {
			log.V(0).Infof("env extraction failed: %v", err)
			return err
		}
		cw_env = string(stdout[:])
		log.V(0).Infof("env: %s", cw_env)

		cmd = exec.Command("buildah", "inspect", "--format='{{.OCIv1.Config.WorkingDir}}'", buildTag)
		stdout, err = cmd.Output()
		if err != nil {
			log.V(0).Infof("workdir extraction failed: %v", err)
			return err
		}
		cw_workdir = strings.ReplaceAll(string(stdout[:]), "'", "")
		log.V(0).Infof("workdir: %s", cw_workdir)

		cw_password = uuid.New().String()
		cmd = exec.Command("/usr/bin/cw-build", buildTag, cw_password)
		stdout, err = cmd.Output()
		if err != nil {
			log.V(0).Infof("cw image generation failed: %v", err)
			return err
		}
		log.V(0).Infof("cwbuild out: %s", stdout)

		opts = dockerclient.BuildImageOptions{
			Context:             ctx,
			Name:                buildTag,
			RmTmpContainer:      true,
			ForceRmTmpContainer: true,
			OutputStream:        os.Stdout,
			Dockerfile:          defaultDockerfilePath,
			NoCache:             false,
			Pull:                s.build.Spec.Strategy.SourceStrategy.ForcePull,
			ContextDir:          "/tmp/cwcontext",
		}

		log.V(0).Infof("generating an image with the encrypted disk")
		err = s.dockerClient.BuildImage(opts)
		if err != nil {
			log.V(0).Infof("image generation failed: %v", err)
			log.V(0).Infof("taking a nap")
			time.Sleep(300 * time.Second)
			return err
		}
	}

	if push {
		if err = tagImage(s.dockerClient, buildTag, pushTag); err != nil {
			return err
		}
		// Get the Docker push authentication
		pushAuthConfig, authPresent := dockercfg.NewHelper().GetDockerAuth(
			pushTag,
			dockercfg.PushAuthType,
		)
		if authPresent {
			log.V(3).Infof("Using provided push secret for pushing %s image", pushTag)
		} else {
			log.V(3).Infof("No push secret provided")
		}
		log.V(0).Infof("\nPushing image %s ...", pushTag)
		startTime := metav1.Now()
		digest, err := s.pushImage(pushTag, pushAuthConfig)

		timing.RecordNewStep(ctx, buildapiv1.StagePushImage, buildapiv1.StepPushImage, startTime, metav1.Now())

		if err != nil {
			s.build.Status.Phase = buildapiv1.BuildPhaseFailed
			s.build.Status.Reason = buildapiv1.StatusReasonPushImageToRegistryFailed
			s.build.Status.Message = builderutil.StatusMessagePushImageToRegistryFailed
			HandleBuildStatusUpdate(s.build, s.client, nil)
			return reportPushFailure(err, authPresent, pushAuthConfig)
		}

		if len(digest) > 0 {
			log.V(0).Infof("digest: %s", digest)
			if cw_password != "" {
				var kernelCmdLine = `KRUN_CFG=2:512 reboot=k panic=-1 panic_print=0 pci=off nomodules console=hvc0 rw no-kvmapf init=/bin/sh virtio_mmio.device=4K@0xd0000000:5 virtio_mmio.device=4K@0xd0001000:6 virtio_mmio.device=4K@0xd0002000:7 virtio_mmio.device=4K@0xd0003000:8 swiotlb=65536 KRUN_PASS=` + cw_password + ` KRUN_INIT=/usr/libexec/s2i/run KRUN_WORKDIR=` + cw_workdir + ` ` + cw_env

				var jsonStr = []byte(`{"sha":"` + digest + `","name":"` + strings.ReplaceAll(pushTag, ":latest", "") + `","kernel_cmd_line":"` + base64.StdEncoding.EncodeToString([]byte(kernelCmdLine)) + `"}`)
				log.V(0).Infof("jsonStr: %s", jsonStr)
				req, err := http.NewRequest("POST", "http://registration-attestation-server.attestation:8080/confidential/register-image", bytes.NewBuffer(jsonStr))
				req.Header.Set("Content-Type", "application/json")

				client := &http.Client{}
				resp, err := client.Do(req)
				if err != nil {
					log.V(0).Infof("error registering image: %v", err)
				}
				defer resp.Body.Close()
			}

			s.build.Status.Output.To = &buildapiv1.BuildStatusOutputTo{
				ImageDigest: digest,
			}
			HandleBuildStatusUpdate(s.build, s.client, nil)
		}
		log.V(0).Infof("Push successful")
	}
	return nil
}

// setupPullSecret provides a Docker authentication configuration when the
// PullSecret is specified.
func (s *S2IBuilder) setupPullSecret() *dockerclient.AuthConfigurations {
	return mergeNodeCredentialsDockerAuth(os.Getenv(dockercfg.PullAuthType))
}

func (s *S2IBuilder) pullImage(name string, searchPaths []string) error {
	log.V(2).Infof("Explicitly pulling image %s", name)
	repository, tag := dockerclient.ParseRepositoryTag(name)
	options := dockerclient.PullImageOptions{
		Repository: repository,
		Tag:        tag,
	}

	if options.Tag == "" && strings.Contains(name, "@") {
		options.Repository = name
	}

	return retryImageAction("Pull", func() (pullErr error) {
		return s.dockerClient.PullImage(options, searchPaths)
	})
}

func (s *S2IBuilder) buildImage(optimization buildapiv1.ImageOptimizationPolicy, opts dockerclient.BuildImageOptions) error {
	return s.dockerClient.BuildImage(opts)
}

func (s *S2IBuilder) pushImage(name string, authConfig dockerclient.AuthConfiguration) (string, error) {
	repository, tag := dockerclient.ParseRepositoryTag(name)
	options := dockerclient.PushImageOptions{
		Name: repository,
		Tag:  tag,
	}
	var err error
	sha := ""
	retryImageAction("Push", func() (pushErr error) {
		sha, err = s.dockerClient.PushImage(options, authConfig)
		return err
	})
	return sha, err
}

// buildEnvVars returns a map with build metadata to be inserted into Docker
// images produced by build. It transforms the output from buildInfo into the
// input format expected by s2iapi.Config.Environment.
// Note that using a map has at least two downsides:
// 1. The order of metadata KeyValue pairs is lost;
// 2. In case of repeated Keys, the last Value takes precedence right here,
//    instead of deferring what to do with repeated environment variables to the
//    Docker runtime.
func buildEnvVars(build *buildapiv1.Build, sourceInfo *git.SourceInfo) s2iapi.EnvironmentList {
	bi := buildInfo(build, sourceInfo)
	envVars := &s2iapi.EnvironmentList{}
	for _, item := range bi {
		envVars.Set(fmt.Sprintf("%s=%s", item.Key, item.Value))
	}
	return *envVars
}

// s2iBuildLabels returns a slice of KeyValue pairs in a format that appendLabel can
// consume.
func s2iBuildLabels(build *buildapiv1.Build, sourceInfo *git.SourceInfo) map[string]string {
	labels := map[string]string{}
	if sourceInfo == nil {
		sourceInfo = &git.SourceInfo{}
	}
	if len(build.Spec.Source.ContextDir) > 0 {
		sourceInfo.ContextDir = build.Spec.Source.ContextDir
	}

	labels = s2iutil.GenerateLabelsFromSourceInfo(labels, toS2ISourceInfo(sourceInfo), builderutil.DefaultDockerLabelNamespace)
	if build != nil && build.Spec.Source.Git != nil && build.Spec.Source.Git.Ref != "" {
		// override the io.openshift.build.commit.ref label to match what we
		// were originally told to check out, as well as the
		// OPENSHIFT_BUILD_REFERENCE environment variable.  This can sometimes
		// differ from git's view (see PotentialPRRetryAsFetch for details).
		labels[builderutil.DefaultDockerLabelNamespace+"build.commit.ref"] = build.Spec.Source.Git.Ref
	}

	// override autogenerated labels
	for _, lbl := range build.Spec.Output.ImageLabels {
		labels[lbl.Name] = lbl.Value
	}
	return labels
}

// scriptProxyConfig determines a proxy configuration for downloading
// scripts from a URL. For now, it uses environment variables passed in
// the strategy's environment. There is no preference given to either lowercase
// or uppercase form of the variable.
func scriptProxyConfig(build *buildapiv1.Build) (*s2iapi.ProxyConfig, error) {
	httpProxy := ""
	httpsProxy := ""
	for _, env := range build.Spec.Strategy.SourceStrategy.Env {
		switch env.Name {
		case "HTTP_PROXY", "http_proxy":
			httpProxy = env.Value
		case "HTTPS_PROXY", "https_proxy":
			httpsProxy = env.Value
		}
	}
	if len(httpProxy) == 0 && len(httpsProxy) == 0 {
		return nil, nil
	}
	config := &s2iapi.ProxyConfig{}
	if len(httpProxy) > 0 {
		proxyURL, err := ParseProxyURL(httpProxy)
		if err != nil {
			return nil, err
		}
		config.HTTPProxy = proxyURL
	}
	if len(httpsProxy) > 0 {
		proxyURL, err := ParseProxyURL(httpsProxy)
		if err != nil {
			return nil, err
		}
		config.HTTPSProxy = proxyURL
	}
	return config, nil
}

// copyToVolumeList copies the artifacts set in the build config to the
// VolumeList struct in the s2iapi.Config
func copyToVolumeList(artifactsMapping []buildapiv1.ImageSourcePath) (volumeList s2iapi.VolumeList) {
	for _, mappedPath := range artifactsMapping {
		volumeList = append(volumeList, s2iapi.VolumeSpec{
			Source:      mappedPath.SourcePath,
			Destination: mappedPath.DestinationDir,
		})
	}
	return
}

func convertS2IFailureType(reason s2iapi.StepFailureReason, message s2iapi.StepFailureMessage) (buildapiv1.StatusReason, string) {
	return buildapiv1.StatusReason(reason), string(message)
}

func isImagePresent(docker DockerClient, imageTag string) bool {
	// TODO: buildah may let us check if image is present without grabbing full JSON
	image, err := docker.InspectImage(imageTag)
	return err == nil && image != nil
}

func getImageLabels(docker DockerClient, imageTag string) (map[string]string, error) {
	image, err := docker.InspectImage(imageTag)
	if err != nil {
		return nil, err
	}
	return image.ContainerConfig.Labels, nil
}

func getAssembleUser(docker DockerClient, imageTag string) (string, error) {
	image, err := docker.InspectImage(imageTag)
	if err != nil {
		return "", err
	}
	// Default the AssembleUser to the most recent User in the builder image
	assembleUser := image.ContainerConfig.User
	if labelAssembleUser, ok := image.ContainerConfig.Labels[s2iconstants.AssembleUserLabel]; ok {
		// If the builder image has the assemble-user label, override with the provided value
		assembleUser = labelAssembleUser
	}
	return extractUser(assembleUser), nil
}

func extractUser(userSpec string) string {
	if strings.Contains(userSpec, ":") {
		parts := strings.SplitN(userSpec, ":", 2)
		return strings.TrimSpace(parts[0])
	}
	return strings.TrimSpace(userSpec)
}
