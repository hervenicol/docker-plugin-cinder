# Changelog

## v0.7.0

* fix label too long errors at mkfs
* Configurable timeouts and delays: timeoutVolumeState, timeoutDeviceWait, delayVolumeState, delayDeviceWait

## v0.6.0

* BREAKING: now creates a directory at the root of the volumes - eases giving specific access rights to a volume, and makes it compatible with rexray volumes. Use `"volumeSubDir": ""` in config for previous behaviour.

## v0.5.0

* Support for "type" volume option (defaults to "classic")
* Config file can set default volume size and default volume type

## v0.4.0

* Switched from virtio to qemu, for OVH compatibility
* configurable filesystem for new volumes
