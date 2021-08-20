package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"strconv"
	"sync"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/docker/go-plugins-helpers/volume"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/volumeattach"
	"github.com/gophercloud/gophercloud/pagination"
)

type plugin struct {
	blockClient   *gophercloud.ServiceClient
	computeClient *gophercloud.ServiceClient
	config        *tConfig
	mutex         *sync.Mutex
}

func newPlugin(provider *gophercloud.ProviderClient, endpointOpts gophercloud.EndpointOpts, config *tConfig) (*plugin, error) {
	blockClient, err := openstack.NewBlockStorageV3(provider, endpointOpts)

	logger := log.WithFields(log.Fields{"action": "newPlugin"})
	logger.Debugf("newPlugin")

	if err != nil {
		return nil, err
	}

	computeClient, err := openstack.NewComputeV2(provider, endpointOpts)

	if err != nil {
		return nil, err
	}

	if len(config.MachineID) == 0 {
		// Find machine ID from Openstack servers

		hostname, err := os.Hostname()
		if err != nil {
			panic(err)
		}

		listOpts := servers.ListOpts{
			 TenantID: config.TenantID,
			 Name: hostname,
		}

		allPages, err := servers.List(computeClient, listOpts).AllPages()
		if err != nil {
			panic(err)
		}

		allServers, err := servers.ExtractServers(allPages)
		if err != nil {
			panic(err)
		}

		if len(allServers) != 1 {
			panic(fmt.Sprintf("Openstack servers list returned more than one server for name %s", hostname))
		}

		for _, server := range allServers {
			log.WithField("id", server.ID).Info("servers list")
		}

		config.MachineID = allServers[0].ID
	} else {
		log.WithField("id", config.MachineID).Debug("Using configured machine ID")
	}

	return &plugin{
		blockClient:   blockClient,
		computeClient: computeClient,
		config:        config,
		mutex:         &sync.Mutex{},
	}, nil
}

func (d plugin) Capabilities() *volume.CapabilitiesResponse {
	logger := log.WithFields(log.Fields{"action": "Capabilities"})
	logger.Debugf("Capabilities")

	return &volume.CapabilitiesResponse{
		Capabilities: volume.Capability{Scope: "global"},
	}
}

func (d plugin) Create(r *volume.CreateRequest) error {
	logger := log.WithFields(log.Fields{"name": r.Name, "action": "create"})
	logger.Infof("Creating volume '%s' ...", r.Name)
	logger.Debugf("Create: %+v", r)

	d.mutex.Lock()
	defer d.mutex.Unlock()

	// DEFAULT SIZE IN GB
	var size = d.config.DefaultSize
	// Default volume type
	var volumeType = d.config.DefaultType
	// No encryption by default
	var encryption = false
	var err error
	keyfile := d.config.EncryptionKey

	if s, ok := r.Options["size"]; ok {
		size = s
	}

	sizeInt, err := strconv.Atoi(size)
	if err != nil {
		logger.WithError(err).Error("Error parsing size option")
		return fmt.Errorf("Invalid size option: %s", err.Error())
	}

	if t, ok := r.Options["type"]; ok {
		volumeType = t
	}

	// if "encryption" option is anything else than "false", it means we want the volume encrypted
	if e, ok := r.Options["encryption"]; ok {
		if strings.ToLower(e) != "false" {
			logger.Debug("Encryption set to true")
			if keyfile == "" {
				logger.Info("Can't encrypt volume, no encryptionKey in config")
			} else {
				encryption = true
			}
		}
	}

	vol, err := volumes.Create(d.blockClient, volumes.CreateOpts{
		Size: sizeInt,
		Name: r.Name,
		VolumeType: volumeType,
	}).Extract()

	if err != nil {
		logger.WithError(err).Errorf("Error creating volume: %s", err.Error())
		return err
	}

	logger.WithField("id", vol.ID).Debug("Volume created")


	// attach & encrypt
	// We must do it here, because Mount() does not have config info
	logger.Debugf("Encryption status: %t", encryption)
	if encryption {
		// attach
		dev, err := attachVolume(&d, r.Name)
		if err != nil {
			logger.WithError(err).Errorf("Error attaching volume: %s", err.Error())
			return err
		}
		// encrypt
		logger.Debugf("Encrypting device %s with key %s", dev, keyfile)
		err = luksFormat(dev, keyfile)
		if err != nil {
			logger.WithError(err).Errorf("Error encrypting volume: %s", err.Error())
			return err
		}

		// detach
		vol, err := d.getByName(r.Name)
		if err != nil {
			logger.WithError(err).Error("Error retrieving volume")
		} else {
			_, err = d.detachVolume(logger.Context, vol)
			if err != nil {
				logger.WithError(err).Error("Error detaching volume")
			}
		}
	}

	return nil
}

func (d plugin) Get(r *volume.GetRequest) (*volume.GetResponse, error) {
	logger := log.WithFields(log.Fields{"name": r.Name, "action": "get"})
	logger.Debugf("Get: %+v", r)

	vol, err := d.getByName(r.Name)

	if err != nil {
		logger.WithError(err).Errorf("Error retrieving volume: %s", err.Error())
		return nil, err
	}

	response := &volume.GetResponse{
		Volume: &volume.Volume{
			Name:       r.Name,
			CreatedAt:  vol.CreatedAt.Format(time.RFC3339),
			Mountpoint: filepath.Join(d.config.MountDir, r.Name, d.config.VolumeSubDir),
		},
	}

	return response, nil
}

func (d plugin) List() (*volume.ListResponse, error) {
	logger := log.WithFields(log.Fields{"action": "list"})
	logger.Debugf("List")

	var vols []*volume.Volume

	pager := volumes.List(d.blockClient, volumes.ListOpts{})
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		vList, _ := volumes.ExtractVolumes(page)

		for _, v := range vList {
			if len(v.Name) > 0 {
				vols = append(vols, &volume.Volume{
					Name:      v.Name,
					CreatedAt: v.CreatedAt.Format(time.RFC3339),
				})
			}
		}

		return true, nil
	})

	if err != nil {
		logger.WithError(err).Errorf("Error listing volume: %s", err.Error())
		return nil, err
	}

	return &volume.ListResponse{Volumes: vols}, nil
}

func (d plugin) Mount(r *volume.MountRequest) (*volume.MountResponse, error) {
	logger := log.WithFields(log.Fields{"name": r.Name, "action": "mount"})
	logger.Infof("Mounting volume '%s' ...", r.Name)
	logger.Debugf("Mount: %+v", r)

	d.mutex.Lock()
	defer d.mutex.Unlock()

	var dev = ""

	physdev, err := attachVolume(&d, r.Name)
	if err != nil {
		logger.WithError(err).Errorf("Error attaching volume: %s", err.Error())
		return nil, err
	}

	// Is it encrypted?
	if result, err := isLuks(physdev); result == true {
		logger.Debugf("Encrypted volume - using key file '%s'", d.config.EncryptionKey)
		// If yes, we must have a passphrase.
		if d.config.EncryptionKey == "" {
			logger.Errorf("Device %s is encrypted, and I have no pass to decrypt it.", physdev)
			return nil, err
		}
		// luksOpen it, or quit with error.
		luksName, err := luksOpen(physdev, d.config.EncryptionKey, r.Name)
		if err != nil {
			logger.WithError(err).Errorf("Opening LUKS device %s as %s with key %s failed", physdev, luksName, d.config.EncryptionKey)
			return nil, err
		}
		// Select dm device
		dev = "/dev/mapper/"+luksName
	} else {
		// or stay on physical device
		dev = physdev
	}


	//
	// Check filesystem and format if needed

	fsType, err := getFilesystemType(dev)
	if err != nil {
		logger.WithError(err).Error("Detecting filesystem type failed")
		return nil, err
	}

	newVolumeFlag := false
	// If not formated:
	if fsType == "" {
		newVolumeFlag = true

		// Format it
		logger.Debug("Volume is empty, formatting")
		if out, err := formatFilesystem(dev, r.Name, d.config.Filesystem); err != nil {
			logger.WithFields(log.Fields{
				"output": out,
				"error": err,
				"filesystem": d.config.Filesystem,
			}).Error("Formatting failed")
			return nil, err
		}
	}

	//
	// Mount device

	path := filepath.Join(d.config.MountDir, r.Name)

	err = createMountDir(path)
	if err != nil {
		logger.WithError(err).Errorf("Error creating mount directory %s", path)
		return nil, err
	}

	logger.WithField("mount", path).Debug("Mounting volume...")
	out, err := exec.Command("mount", dev, path).CombinedOutput()
	if err != nil {
		log.WithError(err).Errorf("%s", out)
		return nil, errors.New(string(out))
	}

	if newVolumeFlag {

		// new volume settings
		var perm = 0700
		var uid = 0
		var gid = 0
		path := filepath.Join(d.config.MountDir, r.Name, d.config.VolumeSubDir)

		logger.Debugf("New volume, creating VolumeSubDir %s, uid %d / gid %d / perm %o", d.config.VolumeSubDir, uid, gid, perm)

		if err = os.MkdirAll(path, os.FileMode(perm)); err != nil {
			logger.WithError(err).Error("Error creating VolumeSubDir")
			return nil, err
		}
		if err = os.Chown(path, uid, gid); err != nil {
			logger.WithError(err).Error("Error creating VolumeSubDir")
			return nil, err
		}
	}

	resp := volume.MountResponse{
		Mountpoint: filepath.Join(path, d.config.VolumeSubDir),
	}

	logger.Debug("Volume successfully mounted")

	return &resp, nil
}

func (d plugin) Path(r *volume.PathRequest) (*volume.PathResponse, error) {
	logger := log.WithFields(log.Fields{"name": r.Name, "action": "path"})
	logger.Debugf("Path: %+v", r)

	resp := volume.PathResponse{
		Mountpoint: filepath.Join(d.config.MountDir, r.Name, d.config.VolumeSubDir),
	}

	return &resp, nil
}

func (d plugin) Remove(r *volume.RemoveRequest) error {
	logger := log.WithFields(log.Fields{"name": r.Name, "action": "remove"})
	logger.Infof("Removing volume '%s' ...", r.Name)
	logger.Debugf("Remove: %+v", r)

	vol, err := d.getByName(r.Name)

	if err != nil {
		logger.WithError(err).Errorf("Error retriving volume: %s", err.Error())
		return err
	}

	logger = logger.WithField("id", vol.ID)

	if len(vol.Attachments) > 0 {
		logger.Debug("Volume still attached, detaching first")
		if vol, err = d.detachVolume(logger.Context, vol); err != nil {
			logger.WithError(err).Error("Error detaching volume")
			return err
		}
	}

	logger.Debug("Deleting block volume...")

	err = volumes.Delete(d.blockClient, vol.ID, volumes.DeleteOpts{}).ExtractErr()
	if err != nil {
		logger.WithError(err).Errorf("Error deleting volume: %s", err.Error())
		return err
	}

	logger.Debug("Volume deleted")

	return nil
}

func (d plugin) Unmount(r *volume.UnmountRequest) error {
	logger := log.WithFields(log.Fields{"name": r.Name, "action": "unmount"})
	logger.Infof("Unmounting volume '%s' ...", r.Name)
	logger.Debugf("Unmount: %+v", r)

	d.mutex.Lock()
	defer d.mutex.Unlock()

	path := filepath.Join(d.config.MountDir, r.Name)

	// find device behind volume and luks volume name (in case it is a luks encrypted volume)
	_, luksName, baseDevice, err := getLuksInfo(path)

	exists, err := isDirectoryPresent(path)
	if err != nil {
		logger.WithError(err).Errorf("Error checking directory stat: %s", path)
	}

	// error with "stats" usually means it exists but we can't reach it
	// that means mounted but broken. So we must unmount it.
	if exists || (err != nil) {
		err = syscall.Unmount(path, 0)
		if err != nil {
			logger.WithError(err).Errorf("Error unmount %s", path)
		}
	}

	// Now the volume is unmounted, we close the luks volume (if it is one):
	if baseDevice != "" {
		if result, _ := isLuks(baseDevice); result == true {
			logger.Debugf("Closing LUKS device %s", luksName)
			luksCloseOutput, err := exec.Command("cryptsetup", "luksClose", luksName).CombinedOutput()
			if err != nil {
				logger.WithError(err).Errorf("Error closing LUKS volume - %s", luksCloseOutput)
			}
		}
	}

	vol, err := d.getByName(r.Name)
	if err != nil {
		logger.WithError(err).Error("Error retrieving volume")
	} else {
		_, err = d.detachVolume(logger.Context, vol)
		if err != nil {
			logger.WithError(err).Error("Error detaching volume")
		}
	}

	return nil
}

func (d plugin) getByName(name string) (*volumes.Volume, error) {
	logger := log.WithFields(log.Fields{"name": name, "action": "getByName"})
	logger.Debugf("GetbyName")

	var volume *volumes.Volume

	pager := volumes.List(d.blockClient, volumes.ListOpts{Name: name})
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		vList, err := volumes.ExtractVolumes(page)

		if err != nil {
			return false, err
		}

		for _, v := range vList {
			if v.Name == name {
				volume = &v
				return false, nil
			}
		}

		return true, nil
	})

	if len(volume.ID) == 0 {
		return nil, errors.New("Not Found")
	}

	return volume, err
}

func (d plugin) detachVolume(ctx context.Context, vol *volumes.Volume) (*volumes.Volume, error) {
	for _, att := range vol.Attachments {
		err := volumeattach.Delete(d.computeClient, att.ServerID, att.ID).ExtractErr()
		if err != nil {
			return nil, err
		}
	}

	return vol, nil
}

func (d plugin) waitOnVolumeState(ctx context.Context, vol *volumes.Volume, status string) (*volumes.Volume, error) {
	if vol.Status == status {
		return vol, nil
	}

	timeout := d.config.TimeoutVolumeState

	for i := 1; i <= timeout; i++ {
		time.Sleep(1000 * time.Millisecond)

		vol, err := volumes.Get(d.blockClient, vol.ID).Extract()
		if err != nil {
			return nil, err
		}

		if vol.Status == status {
			time.Sleep(time.Duration(d.config.DelayVolumeState) * time.Second)
			return vol, nil
		}
	}

	log.WithContext(ctx).Debugf("Volume did not become %s: %+v", status, vol)

	return nil, fmt.Errorf("Volume status became %s", vol.Status)
}
