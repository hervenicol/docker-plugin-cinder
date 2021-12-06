package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"bufio"
	"syscall"

	log "github.com/sirupsen/logrus"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/volumeattach"

)

func getFilesystemType(dev string) (string, error) {
	out, err := exec.Command("blkid", "-s", "TYPE", "-o", "value", dev).CombinedOutput()

	if err != nil {
		if len(out) == 0 {
			return "", nil
		}

		return "", errors.New(string(out))
	}

	return string(out), nil
}

// Retrieves info for a LUKS-encrypted volume
// parameters:
// - mount path
// returns:
// - device name (/dev/mapper/luksdevice)
// - luks name (luksdevice)
// - base device name (/dev/sdb)
// - error
// When the volume is not LUKS, returns empty values.
// if "error" contains something, that's a real error !
func getLuksInfo(mountPath string) (string, string, string, error) {
	mountDevice := ""
	baseDevice := ""

	logger := log.WithFields(log.Fields{"mountPath": mountPath, "action": "getLuksInfo"})

	// /proc/mounts lists all current mounts
	procsMount := "/proc/mounts"

	// Open list of current mounts
	f, err := os.Open(procsMount)
	if err != nil {
		return "", "", "", errors.New(fmt.Sprintf("Failed opening %s - %s", procsMount, err))
	}
	defer func() {
		if err = f.Close(); err != nil {
			os.Exit(1)
		}
	}()

	// read line by line
	// format: [device] [mountpath] [other info we don't care about]
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		testArray := strings.Fields(scanner.Text())
		if testArray[1] == mountPath {
			// mount found !
			mountDevice = testArray[0]
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return "", "", "", errors.New(fmt.Sprintf("Error scanning %s contents: %s", procsMount, err))
	}
	// fail if no mount found
	if mountDevice == "" {
		return "", "", "", errors.New(fmt.Sprintf("mount %s not found in %s", mountPath, procsMount))
	}

	// device should start with /dev/mapper - keep the part that is after
	if strings.HasPrefix(mountDevice, "/dev/mapper/") != true {
		logger.Debugf("Not a /dev/mapper device, not LUKS")
		// That is not an error, most devices are not LUKS
		return "", "", "", nil
	}
	luksName := strings.TrimPrefix(mountDevice, "/dev/mapper/")

	// status shows us the base block device path
	cryptStatusOut, err := exec.Command("cryptsetup", "status", luksName).CombinedOutput()
	if err != nil {
		return "", "", "", errors.New(fmt.Sprintf("Error executing cryptsetup - %s", err))
	}
	// read line by line, look for "device:"
	scanner = bufio.NewScanner(strings.NewReader(string(cryptStatusOut,)))
	for scanner.Scan() {
		testArray := strings.Fields(scanner.Text())
		if testArray[0] == "device:" {
			baseDevice = testArray[1]
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", "", errors.New("Error scanning cryptsetup output")
	}
	// fail if no device found
	if baseDevice == "" {
		logger.Debugf("No \"Device:\" line found in cryptsetup output - probably not a LUKS device")
		// again, not an error to not be LUKS
		return "", "", "", nil
	}

	// All went well, here is the retrieved info
	logger.Debugf("Mount found for '%s' - device '%s' - luks name '%s' - base device '%s'", mountPath, mountDevice, luksName, baseDevice)
	return mountDevice, luksName, baseDevice, nil
}

func isLuks(dev string) (status bool, err error) {
	logger := log.WithFields(log.Fields{"dev": dev, "action": "isLuks"})

	execOut, err := exec.Command("cryptsetup", "isLuks", dev).CombinedOutput()
	if err != nil {
		if len(execOut) > 0 {
			logger.Errorf("isLuks command failed - %s", execOut)
		}
		return false, err
	}
	return true, err
}

func luksOpen(devName string, keyfile string, volumeName string) (luksName string, err error) {
	logger := log.WithFields(log.Fields{"dev": devName, "key": keyfile, "action": "luksOpen"})

	luksName = volumeName+"_luks"
	cmd := exec.Command("cryptsetup", "luksOpen", "-d", keyfile, devName, luksName )

	execOut, err := cmd.CombinedOutput()
	if err != nil {
		if len(execOut) > 0 {
			logger.Errorf("luksOpen command failed - %s", execOut)
		}
		return "", err
	}

	return luksName, err
}

func luksFormat(devName string, keyfile string) (error) {
	logger := log.WithFields(log.Fields{"dev": devName, "key": keyfile, "action": "luksOpen"})

	cmd := exec.Command("cryptsetup", "luksFormat", "-q" ,"-d", keyfile, devName )

	execOut, err := cmd.CombinedOutput()
	if err != nil {
		if len(execOut) > 0 {
			logger.Errorf("luksFormat command failed - %s", execOut)
		}
		return err
	}

	return nil
}

// Attach a volume to current instance
// Input:
// * driver
// * volume name
// Output:
// * device name
// * error
func attachVolume(d *plugin, volumeName string) (string, error) {

	logger := log.WithFields(log.Fields{"name": volumeName, "action": "attachVolume"})
	logger.Infof("Attaching volume '%s' ...", volumeName)

	vol, err := d.getByName(volumeName)
	if err != nil {
		logger.WithError(err).Errorf("Error retrieving volume: %s", err.Error())
		return "", err
	}

	logger = logger.WithField("id", vol.ID)

	if vol.Status == "creating" || vol.Status == "detaching" {
		logger.Infof("Volume is in '%s' state, wait for 'available'...", vol.Status)
		if vol, err = d.waitOnVolumeState(logger.Context, vol, "available"); err != nil {
			logger.Error(err.Error())
			return "", err
		}
	}

	if vol, err = volumes.Get(d.blockClient, vol.ID).Extract(); err != nil {
		return "", err
	}

	if len(vol.Attachments) > 0 {
		logger.Debug("Volume already attached, detaching first")
		if vol, err = d.detachVolume(logger.Context, vol); err != nil {
			logger.WithError(err).Error("Error detaching volume")
			return "", err
		}

		if vol, err = d.waitOnVolumeState(logger.Context, vol, "available"); err != nil {
			logger.WithError(err).Error("Error detaching volume")
			return "", err
		}
	}

	if vol.Status != "available" {
		logger.Debugf("Volume: %+v\n", vol)
		logger.Errorf("Invalid volume state for mounting: %s", vol.Status)
		return "", errors.New("Invalid Volume State")
	}

	//
	// Attaching block volume to compute instance

	opts := volumeattach.CreateOpts{VolumeID: vol.ID}
	logger.Debugf("Attaching volume %s to Machine %s", vol.ID, d.config.MachineID)
	_, err = volumeattach.Create(d.computeClient, d.config.MachineID, opts).Extract()

	if err != nil {
		logger.WithError(err).Errorf("Error attaching volume: %s", err.Error())
		return "", err
	}

	//
	// Waiting for device appearance

	// ID is sometimes truncated in device filename
	devid := fmt.Sprintf("%.20s", vol.ID)
	devpath := "/dev/disk/by-id"
	logger.WithField("devid", devid).Debug("Waiting for device to appear...")
	dev, err := waitForDevice(devpath, devid, d.config.TimeoutDeviceWait)
	time.Sleep(time.Duration(d.config.DelayDeviceWait) * time.Second)
	logger.WithField("dev", dev).Debug("Device found")

	if err != nil {
		logger.WithError(err).Error("Expected block device not found")
		return "", fmt.Errorf("Block device not found: %s", devid)
	}

	return dev, nil
}


func formatFilesystem(dev string, label string, filesystem string) (string, error) {
	mkfsBin := fmt.Sprintf("mkfs.%s", filesystem)
	if len(label) > 12 {
		label=label[:12]
	}

	out, err := exec.Command(mkfsBin, "-L", label, dev).CombinedOutput()

	if err != nil {
		return string(out), errors.New(fmt.Sprintf("Command: '%s -L %s %s' - err: '%s'", mkfsBin, label, dev, err))
	}

	return "", nil
}

// look for a device which name contains id, under dir
// and return the full path+filename
func waitForDevice(dir string, id string, timeout int) (string, error) {

	for i := 0; i <= timeout; i++ {

		files, err := os.ReadDir(dir)
		if err != nil {
			return "", err
		}

		for _, file := range files {
			if strings.Contains(file.Name(), id) {
				return fmt.Sprintf("%s/%s", dir, file.Name()), nil
			}
		}

		time.Sleep(1 * time.Second)
	}

	return "", fmt.Errorf("Timeout waiting for file: %s", id)
}

func isDirectoryPresent(path string) (bool, error) {
	stat, err := os.Stat(path)

	if os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	} else {
		return stat.IsDir(), nil
	}
}

func createMountDir(path string) (error) {
	// Sometimes mkdir fails, and I've observed it is a symptom of a bug
	// where volume is half-mounted (?)
	// this can be solved with umount
	// (anyway the volume should not be mounted at this point)

	// as I suspect this "half-mounted" problem comes from a race condition
	// where unmount of the previous container and mount of the new container
	// may be too fast (or maybe at the same time?),
	// I prefer to wait a bit before retrying the unmount.

	logger := log.WithFields(log.Fields{"action": "createMountDir"})
	sleep := 1 * time.Second
	for retry := 0; retry < 3; retry++ {

		// If mkdir is OK, proceed to next step
		if err := os.MkdirAll(path, 0700); err == nil {
			return nil
		}

		// exponential backoff
		time.Sleep(sleep)
		sleep = sleep * 2

		err := syscall.Unmount(path, 0)
		if err != nil {
			logger.WithError(err).Errorf("Error unmount %s", path)
		}
	}
	return fmt.Errorf("Failed creating directory %s", path)
}
