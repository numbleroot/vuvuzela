package eval

import (
	"encoding/json"
	"fmt"
	"io/ioutil"

	"vuvuzela.io/alpenhorn/config"
)

// StaticConfig returns a prepared static
// service configuration for evaluation purposes.
func StaticConfig() (*config.SignedConfig, error) {

	// Read JSON configuration file from specific
	// file system location.
	data, err := ioutil.ReadFile("./world.json")
	if err != nil {
		return nil, fmt.Errorf("failed to read service information from file: %v", err)
	}

	// Reserve memory for config.
	conf := &config.SignedConfig{}

	// Unmarshal into alpenhorn.config.SignedConfig.
	err = json.Unmarshal(data, conf)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal service information into config struct: %v", err)
	}

	return conf, nil
}
