package machine

import (
	"encoding/json"
	"os"

	igntypes "github.com/coreos/ignition/config/v2_2/types"
	"github.com/pkg/errors"

	"github.com/openshift/installer/pkg/asset"
	"github.com/openshift/installer/pkg/asset/installconfig"
	"github.com/openshift/installer/pkg/asset/tls"
)

const (
	masterIgnFilename = "master.ign"
)

// Master is an asset that generates the ignition config for master nodes.
type Master struct {
	Config *igntypes.Config
	File   *asset.File
}

var _ asset.WritableAsset = (*Master)(nil)

// Dependencies returns the assets on which the Master asset depends.
func (a *Master) Dependencies() []asset.Asset {
	return []asset.Asset{
		&installconfig.InstallConfig{},
		&tls.RootCA{},
	}
}

// Generate generates the ignition config for the Master asset.
func (a *Master) Generate(dependencies asset.Parents) error {
	installConfig := &installconfig.InstallConfig{}
	rootCA := &tls.RootCA{}
	dependencies.Get(installConfig, rootCA)

	a.Config = pointerIgnitionConfig(installConfig.Config, rootCA.Cert(), "master")

	data, err := json.Marshal(a.Config)
	if err != nil {
		return errors.Wrap(err, "failed to marshal Ignition config")
	}
	a.File = &asset.File{
		Filename: masterIgnFilename,
		Data:     data,
	}

	return nil
}

// Name returns the human-friendly name of the asset.
func (a *Master) Name() string {
	return "Master Ignition Config"
}

// Files returns the files generated by the asset.
func (a *Master) Files() []*asset.File {
	if a.File != nil {
		return []*asset.File{a.File}
	}
	return []*asset.File{}
}

// Load returns the master ignitions from disk.
func (a *Master) Load(f asset.FileFetcher) (found bool, err error) {
	file, err := f.FetchByName(masterIgnFilename)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	config := &igntypes.Config{}
	if err := json.Unmarshal(file.Data, config); err != nil {
		return false, errors.Wrapf(err, "failed to unmarshal")
	}

	a.File, a.Config = file, config
	return true, nil
}
