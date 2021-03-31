# docker-plugin-cinder

This Docker volume plugin for utilizing OpenStack Cinder for persistent storage volumes.

The plugin attaches block storage volumes to the compute instance running the plugin. If the volume is already attached to another compute instance it will be detached first.


## Requirements

Tested on OVH Public Cloud Openstack

* Block Storage API v3
* Compute API v2
* QEMU


## Build

```
docker run -ti --rm -v "$(pwd)":/go/docker-plugin-cinder -w /go/docker-plugin-cinder golang:1.16 go build -o docker-plugin-cinder
```


## Setup - interactive

Provide configuration for the plugin:

```
{
    "endpoint": "http://keystone.example.org/v3",
    "username": "username",
    "password": "password",
    "domainID: "",
    "domainName": "default"
    "tenantID": "",
    "tenantName": "",
    "applicationCredentialId": "",
    "applicationCredentialName": "",
    "applicationCredentialSecret": "",
    "region": "",
    "mountDir": "/var/lib/cinder/mounts",
    "filesystem": "",
    "defaultsize": "",
    "defaulttype": ""
}
```

Run the daemon before docker:

```
$ ./docker-plugin-cinder -config config.json
INFO Connecting...                                 endpoint="http://api.os.xopic.de:5000/v3"
INFO servers list                                  id=dadfaf91-dbfc-492c-8701-1de57b998817
INFO Connected.                                    endpoint="http://api.os.xopic.de:5000/v3"
```

By default a `cinder.json` from the current working directory will be used.


## Run as a systemd service

* Create your configuration file as `/etc/docker/cinder.json`
* Copy `docker-plugin-cinder` as `/usr/local/bin/docker-plugin-cinder`
* Create workdir: `mkdir -p /var/lib/cinder/mounts`
* Use example/docker-plugin-cinder.service as systemd unit file:
  * `cp example/docker-plugin-cinder.service /etc/systemd/system/docker-plugin-cinder.service` 
  * `chmod 644 /etc/systemd/system/docker-plugin-cinder.service`
  * `systemctl daemon-reload`
  * `systemctl enable docker-plugin-cinder`

## Run as a docker plugin

... yet to be written ...


## Usage

The default volume size can be set in config, but can be overridden per volume:

```
$ docker volume create -d cinder -o size=20 volname
```

Same for volume type:

```
$ docker volume create -d cinder -o type=high-speed volname
```


## Notes

### Machine ID

Original plugin was relying on `/etc/machine-id`. This version does not. Instead, it serches in Openstack servers list, based on the machine's hostname.
But you can force your server's ID with `machineID` in the configuration file.

### Attaching volumes

Requested volumes that are already attached will be forcefully detached and moved to the requesting machine.


## License

MIT License
