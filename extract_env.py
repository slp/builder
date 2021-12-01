import sys
import json
import subprocess

result = subprocess.run(['buildah', 'inspect', sys.argv[1]], stdout=subprocess.PIPE)
data = json.loads(result.stdout)
config = json.loads(data['Config'])
env = [ ]
for i in config['config']['Env']:
    name, value = i.split('=')
    if name != "DESCRIPTION" and name != "SUMMARY":
        env.append("%s=\"%s\"" % (name, value))

print(" ".join(env), end='')

