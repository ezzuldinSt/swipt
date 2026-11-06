package automation

import (
	"encoding/json"
	"os"
	"path/filepath"

	E "github.com/sagernet/sing/common/exceptions"
)

type ActionInfo struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description,omitempty"`
	Expose      []Caller        `json:"expose,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	Output      json.RawMessage `json:"output,omitempty"`
}

type Manifest struct {
	ID             string
	Name           string
	LocalizedNames map[string]string
	Description    string
	Version        string
	VersionCode    int64
	Icon           Icon
	UpdateURL      string
	Interface      bool
	Service        bool
	Actions        []ActionInfo
	fields         map[string]json.RawMessage
}

type manifestFields struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	LocalizedNames map[string]string `json:"localizedNames"`
	Description    string            `json:"description"`
	Version        string            `json:"version"`
	VersionCode    int64             `json:"versionCode"`
	Icon           Icon              `json:"icon"`
	UpdateURL      string            `json:"updateURL"`
	Interface      bool              `json:"interface"`
	Service        bool              `json:"service"`
	Actions        []ActionInfo      `json:"actions"`
}

func ParseManifest(data []byte) (Manifest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Manifest{}, E.Cause(err, "read ", ManifestFile)
	}
	known := manifestFields{Icon: DefaultIcon}
	if err := json.Unmarshal(data, &known); err != nil {
		return Manifest{}, E.Cause(err, "read ", ManifestFile)
	}
	if known.Icon.Check() != nil {
		known.Icon = DefaultIcon
	}
	return Manifest{
		ID: known.ID, Name: known.Name, LocalizedNames: known.LocalizedNames, Description: known.Description, Version: known.Version,
		VersionCode: known.VersionCode, Icon: known.Icon, UpdateURL: known.UpdateURL, Interface: known.Interface, Service: known.Service,
		Actions: known.Actions, fields: fields,
	}, nil
}

func IsNewer(remote Manifest, version string, versionCode int64) bool {
	if remote.VersionCode > 0 && versionCode > 0 {
		return remote.VersionCode > versionCode
	}
	return remote.Version != "" && remote.Version != version
}

func EncodeManifest(manifest Manifest) ([]byte, error) {
	out := make(map[string]any, len(manifest.fields)+10)
	for key, value := range manifest.fields {
		out[key] = value
	}
	set := func(key string, value any, present bool) {
		if present {
			out[key] = value
		} else {
			delete(out, key)
		}
	}
	for key, value := range map[string]string{
		"id": manifest.ID, "name": manifest.Name, "description": manifest.Description, "version": manifest.Version, "updateURL": manifest.UpdateURL,
	} {
		set(key, value, value != "")
	}
	set("versionCode", manifest.VersionCode, manifest.VersionCode > 0)
	set("localizedNames", manifest.LocalizedNames, len(manifest.LocalizedNames) > 0)
	set("icon", manifest.Icon, manifest.Icon != DefaultIcon)
	set("interface", true, manifest.Interface)
	set("service", true, manifest.Service)
	set("actions", manifest.Actions, len(manifest.Actions) > 0)
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func LoadManifest(directory string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(directory, ManifestFile))
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(data)
}
