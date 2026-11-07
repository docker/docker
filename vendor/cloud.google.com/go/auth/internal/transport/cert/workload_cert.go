// Copyright 2024 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cert

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/googleapis/enterprise-certificate-proxy/client/util"
)

type certConfigs struct {
	Workload *workloadSource `json:"workload"`
}

type workloadSource struct {
	CertPath string `json:"cert_path"`
	KeyPath  string `json:"key_path"`
}

type certificateConfig struct {
	CertConfigs certConfigs `json:"cert_configs"`
}

type certConfigInfo struct {
	certPath string // path to the certificate file
	keyPath  string // path to the private key file
	useECP   bool   // whether to use Enterprise Certificate Proxy
}

// GetConfigFilePath determines the path to the certificate configuration file.
// It first checks for the presence of an environment variable that specifies
// the file path. If the environment variable is not set, it falls back to
// a default configuration file path. If a non-empty configFilePath is provided,
// it is returned.
func GetConfigFilePath(configFilePath string) string {
	if configFilePath != "" {
		return configFilePath
	}
	envFilePath := util.GetConfigFilePathFromEnv()
	if envFilePath != "" {
		return envFilePath
	}
	return util.GetDefaultConfigFilePath()

}

// GetFileBasedCertificatePath retrieves the certificate file path from the provided
// configuration file. If the configFilePath is empty, it attempts to load
// the configuration from a well-known gcloud location.
// This function is exposed to allow other packages, such as the
// externalaccount package, to retrieve the certificate path without needing
// to load the entire certificate configuration.
func GetFileBasedCertificatePath(configFilePath string) (string, error) {
	configFilePath = GetConfigFilePath(configFilePath)
	info, err := parseCertConfig(configFilePath)
	if err != nil {
		return "", err
	}
	if info.useECP {
		return "", errors.New("enterprise certificate proxy is enabled, certificate path is not available")
	}
	return info.certPath, nil
}

// IsECPConfig checks if the given configuration is an ECP certificate configuration.
func IsECPConfig(configFilePath string) bool {
	configFilePath = GetConfigFilePath(configFilePath)
	info, err := parseCertConfig(configFilePath)
	if err != nil {
		return false
	}
	return info.useECP
}

// NewWorkloadX509CertProvider creates a certificate source
// that reads a certificate and private key file from the local file system.
// This is intended to be used for workload identity federation.
//
// The configFilePath points to a config file containing relevant parameters
// such as the certificate and key file paths.
// If configFilePath is empty, the client will attempt to load the config from
// a well-known gcloud location.
func NewWorkloadX509CertProvider(configFilePath string) (Provider, error) {
	configFilePath = GetConfigFilePath(configFilePath)
	info, err := parseCertConfig(configFilePath)
	if err != nil {
		return nil, err
	}

	if info.useECP {
		return NewEnterpriseCertificateProxyProvider(configFilePath)
	}

	source := &workloadSource{
		CertPath: info.certPath,
		KeyPath:  info.keyPath,
	}
	return source.getClientCertificate, nil
}

// getClientCertificate attempts to load the certificate and key from the files specified in the
// certificate config.
func (s *workloadSource) getClientCertificate(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(s.CertPath, s.KeyPath)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

// parseCertConfig attempts to read the provided config file and return the certificate, private
// key file paths, and a boolean indicating whether to use ECP inside a certConfigInfo struct.
func parseCertConfig(configFilePath string) (certConfigInfo, error) {
	jsonFile, err := os.Open(configFilePath)
	if err != nil {
		return certConfigInfo{}, errSourceUnavailable
	}
	defer jsonFile.Close()

	byteValue, err := io.ReadAll(jsonFile)
	if err != nil {
		return certConfigInfo{}, err
	}

	var config certificateConfig
	if err := json.Unmarshal(byteValue, &config); err != nil {
		return certConfigInfo{}, err
	}

	if config.CertConfigs.Workload == nil {
		// If 'workload' field is absent, it is an ECP config.
		// Validate that the config file is a valid ECP file.
		var rawMap map[string]any
		if err := json.Unmarshal(byteValue, &rawMap); err != nil {
			return certConfigInfo{}, err
		}
		certConfigs, ok := rawMap["cert_configs"].(map[string]any)
		if !ok {
			return certConfigInfo{}, errSourceUnavailable
		}
		hasECPSection := false
		for _, section := range []string{"pkcs11", "windows_store", "macos_keychain"} {
			if _, exists := certConfigs[section]; exists {
				hasECPSection = true
				break
			}
		}
		_, hasLibs := rawMap["libs"]
		if !hasECPSection || !hasLibs {
			return certConfigInfo{}, errSourceUnavailable
		}
		return certConfigInfo{useECP: true}, nil
	}

	certFile := config.CertConfigs.Workload.CertPath
	keyFile := config.CertConfigs.Workload.KeyPath

	if certFile == "" {
		return certConfigInfo{}, errors.New("certificate configuration is missing the certificate file location")
	}

	if keyFile == "" {
		return certConfigInfo{}, errors.New("certificate configuration is missing the key file location")
	}

	return certConfigInfo{certPath: certFile, keyPath: keyFile}, nil
}
