/*
Copyright (C) 2017 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package image

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/containers/image/copy"
	"github.com/containers/image/oci/layout"
	"github.com/containers/image/signature"
	"github.com/containers/image/transports/alltransports"
	"github.com/containers/image/types"
	"github.com/docker/machine/libmachine/drivers"
	"github.com/minishift/minishift/pkg/minikube/sshutil"
	"github.com/minishift/minishift/pkg/util"
	"github.com/minishift/minishift/pkg/util/filehelper"
	"github.com/minishift/minishift/pkg/util/progressdots"
	"golang.org/x/crypto/ssh"
	"io/ioutil"
	"path/filepath"
	"strings"
)

// OciImageHandler is an ImageHandler implementation using OCI format to maintain the local cache.
type OciImageHandler struct {
	driver               drivers.Driver
	dockerClientSettings *dockerClientConfig
}

type dockerClientConfig struct {
	DockerHost      string
	DockerCertPath  string
	DockerTLSVerify bool
}

type Index struct {
	Manifests     Manifests     `json:"manifests"`
	SchemaVersion SchemaVersion `json:"schemaVersion"`
}

type SchemaVersion int

type Manifests []Manifest

type Manifest struct {
	Annotations Annotations `json:"annotations"`
	Digest      string      `json:"digest"`
	MediaType   string      `json:"mediaType"`
	Platform    Platform    `json:"platform"`
	Size        int64       `json:"size"`
}

type Annotations struct {
	Name string `json:"org.opencontainers.image.ref.name"`
}

type Platform struct {
	Architecture string `json:"architecture"`
	Os           string `json:"os"`
}

// NewOciImageHandler creates a new ImageHandler which stores cached images in OCI format.
// It takes a reference to a Driver in order to communicate with the VM and Docker and a map containing the environment settings for the Minishift Docker daemon.
func NewOciImageHandler(driver drivers.Driver, dockerEnv map[string]string) (*OciImageHandler, error) {
	settings, err := getDockerSettings(dockerEnv)
	if err != nil {
		return nil, err
	}
	return &OciImageHandler{driver: driver, dockerClientSettings: settings}, nil
}

// NewLocalOnlyOciImageHandler creates  a new ImageHandler which can only interact with the local cache.
// No connection information to the Docker daemon are provided. Functions interacting with the Docker daemon will fail.
func NewLocalOnlyOciImageHandler() (*OciImageHandler, error) {
	return &OciImageHandler{driver: nil, dockerClientSettings: nil}, nil
}

// ImportImages imports cached images from the host into the Docker daemon of the VM.
func (handler *OciImageHandler) ImportImages(config *ImageCacheConfig) ([]string, error) {
	out := handler.getOutputWriter(config)
	importedImages := []string{}

	policyContext, err := handler.getPolicyContext()
	if err != nil {
		return importedImages, fmt.Errorf("Error creating security context: %s", err.Error())
	}

	availableImages, err := handler.GetDockerImages()
	if err != nil {
		return importedImages, err
	}

	multiError := util.MultiError{}
	for _, imageName := range config.CachedImages {
		fmt.Fprint(out, fmt.Sprintf("   Importing '%s' ", imageName))
		progressDots := progressdots.New()
		progressDots.SetWriter(out)
		progressDots.Start()
		if _, found := availableImages[imageName]; found {
			handler.endProgress(progressDots, out, OK)
			importedImages = append(importedImages, imageName)
			continue

		}

		if !handler.IsImageCached(config, imageName) {
			handler.endProgress(progressDots, out, CACHE_MISS)
			continue
		}

		err := handler.importImage(imageName, config, policyContext, out)
		handler.endProgress(progressDots, out, handler.progressStatusForError(err))
		multiError.Collect(err)
		if err == nil {
			importedImages = append(importedImages, imageName)
		}
	}
	return importedImages, multiError.ToError()
}

// ExportImages exports the images specified as part of the ImageCacheConfig from the VM to the host.
func (handler *OciImageHandler) ExportImages(config *ImageCacheConfig, overwrite bool) ([]string, error) {
	out := handler.getOutputWriter(config)
	exportedImages := []string{}

	policyContext, err := handler.getPolicyContext()
	if err != nil {
		return exportedImages, fmt.Errorf("Error creating security context: %s", err.Error())
	}

	multiError := util.MultiError{}
	for _, imageName := range config.CachedImages {
		fmt.Fprint(out, fmt.Sprintf("Exporting '%s'", imageName))
		err = nil
		progressDots := progressdots.New()
		progressDots.SetWriter(out)
		progressDots.Start()

		if !handler.IsImageCached(config, imageName) || overwrite {
			err = handler.exportImage(imageName, config, policyContext, out, overwrite)
		}
		handler.endProgress(progressDots, out, handler.progressStatusForError(err))
		multiError.Collect(err)
		if err != nil {
			exportedImages = append(exportedImages, imageName)
		}
	}

	return exportedImages, multiError.ToError()
}

// PruneImages delete the specified as command line option.
func (handler *OciImageHandler) PruneImages(config *ImageCacheConfig) ([]string, error) {
	out := handler.getOutputWriter(config)
	PruneImages := []string{}

	multiError := util.MultiError{}
	for _, imageName := range config.CachedImages {
		fmt.Fprint(out, fmt.Sprintf("Deleting '%s' from the local cache", imageName))
		var err error
		progressDots := progressdots.New()
		progressDots.SetWriter(out)
		progressDots.Start()
		if !handler.IsImageCached(config, imageName) {
			return nil, fmt.Errorf("Image %s is not cached", imageName)
		}
		err = handler.pruneImage(imageName, config)
		handler.endProgress(progressDots, out, handler.progressStatusForError(err))
		multiError.Collect(err)
		if err != nil {
			PruneImages = append(PruneImages, imageName)
		}
	}

	return PruneImages, multiError.ToError()
}

// IsImageCached returns true if the specified image is cached, false otherwise.
func (handler *OciImageHandler) IsImageCached(config *ImageCacheConfig, image string) bool {
	cachedImages := handler.GetCachedImages(config)
	_, found := cachedImages[image]
	return found
}

// AreImagesCached returns true if all images specified in the config are cached, false otherwise.
func (handler *OciImageHandler) AreImagesCached(config *ImageCacheConfig) bool {
	cachedImages := handler.GetCachedImages(config)

	for _, image := range config.CachedImages {
		if _, found := cachedImages[image]; !found {
			return false
		}
	}

	return true
}

func (handler *OciImageHandler) GetCachedImages(config *ImageCacheConfig) map[string]bool {
	cachedImages := make(map[string]bool)

	index, err := handler.getIndex(config.HostCacheDir)
	if index == nil || err != nil {
		return cachedImages
	}

	for _, manifest := range index.Manifests {
		cachedImages[manifest.Annotations.Name] = true
	}

	return cachedImages
}

func (handler *OciImageHandler) GetDockerImages() (map[string]bool, error) {
	dockerImages := make(map[string]bool)

	session, err := handler.createSSHSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	// We don't want to have an image which have <none> tag in our output list
	// Also in case there is no image in docker daemon then exit code should be 0 instead 1
	// and `|| true` is used.
	// which refer to dangling image for docker.
	cmd := "docker images --format '{{.Repository}}:{{.Tag}}' | grep -vw '<none>' || true"
	var buffer bytes.Buffer
	session.Stdout = &buffer
	err = session.Run(cmd)
	if err != nil {
		return nil, fmt.Errorf("Error running command '%s': %v", cmd, err)
	}

	for _, image := range strings.Split(buffer.String(), "\n") {
		if len(image) > 0 {
			dockerImages[image] = true
		}
	}

	return dockerImages, nil
}

func (handler *OciImageHandler) pullImage(image string, out io.Writer) error {
	session, err := handler.createSSHSession()
	if err != nil {
		return err
	}
	defer session.Close()

	cmd := fmt.Sprintf("docker pull %s", image)
	cmdOut, err := session.CombinedOutput(cmd)
	if err != nil {
		return fmt.Errorf("Error running command '%s': %v \n%s", cmd, err, string(cmdOut[:]))
	}

	return nil
}

func (handler *OciImageHandler) getIndex(cacheDir string) (*Index, error) {
	indexPath := filepath.Join(cacheDir, "index.json")
	if !filehelper.Exists(indexPath) {
		return nil, nil
	}

	raw, err := ioutil.ReadFile(indexPath)
	if err != nil {
		return nil, err
	}

	var index Index
	err = json.Unmarshal(raw, &index)
	if err != nil {
		return nil, err
	}

	return &index, nil
}

func (handler *OciImageHandler) importImage(image string, config *ImageCacheConfig, policyContext *signature.PolicyContext, out io.Writer) error {
	srcRef, err := layout.NewReference(config.HostCacheDir, image)
	if err != nil {
		return fmt.Errorf("Invalid image source '%v': %v", srcRef, err)
	}

	destRef, err := alltransports.ParseImageName(fmt.Sprintf("docker-daemon:%s", image))
	if err != nil {
		return fmt.Errorf("Invalid image source '%s': %v", image, err)
	}

	err = handler.copyImage(srcRef, destRef, policyContext, config.HostCacheDir)
	if err != nil {
		return err
	}

	return nil
}

func (handler *OciImageHandler) exportImage(image string, config *ImageCacheConfig, policyContext *signature.PolicyContext, out io.Writer, overwrite bool) error {
	availableImages, err := handler.GetDockerImages()
	if err != nil {
		return err
	}

	if _, found := availableImages[image]; !found || overwrite {
		err := handler.pullImage(image, config.Out)
		if err != nil {
			return err
		}
	}

	srcRef, err := alltransports.ParseImageName(fmt.Sprintf("docker-daemon:%s", image))
	if err != nil {
		return fmt.Errorf("Invalid image source '%s': %v", image, err)
	}

	// ImageIndexLocation should be a directory location which will be atomic for each image.
	// for an image "openshift/origin-control-plane:v3.10.0"
	// it will be $HOME/.minishift/cache/image/openshift-origin-control-plane-v3.10.0
	r := strings.NewReplacer(":", "-", "/", "-")
	ImageIndexLocation := filepath.Join(config.HostCacheDir, r.Replace(image))

	if _, err := os.Stat(ImageIndexLocation); !os.IsNotExist(err) {
		return fmt.Errorf("Importing %s is already in progress", image)
	}

	destRef, err := layout.NewReference(ImageIndexLocation, image)
	if err != nil {
		return fmt.Errorf("Invalid image destination '%v': %v", destRef, err)
	}

	err = handler.copyImage(srcRef, destRef, policyContext, config.HostCacheDir)
	if err != nil {
		return err
	}

	// Get index of current export image
	pulledImageIndex, err := handler.getIndex(ImageIndexLocation)
	if err != nil {
		return err
	}

	// Remove tmpImageIndexLocation
	if err := os.RemoveAll(ImageIndexLocation); err != nil {
		return err
	}

	// Get index of already available image
	availableImageIndex, err := handler.getIndex(config.HostCacheDir)
	if err != nil {
		return err
	}

	if availableImageIndex == nil {
		availableImageIndex = &Index{Manifests: Manifests{}, SchemaVersion: 2}
	}

	availableImageIndex.Manifests = append(availableImageIndex.Manifests, pulledImageIndex.Manifests...)

	if err := handler.updateIndex(config.HostCacheDir, availableImageIndex); err != nil {
		return err
	}

	return nil
}

func (handler *OciImageHandler) pruneImage(image string, config *ImageCacheConfig) error {
	index, err := handler.getIndex(config.HostCacheDir)
	if index == nil || err != nil {
		return err
	}

	for i, manifest := range index.Manifests {
		if manifest.Annotations.Name == image {
			if err := os.RemoveAll(filepath.Join(config.HostCacheDir, "blobs", "sha256", strings.TrimPrefix(manifest.Digest, "sha256:"))); err != nil {
				return err
			}
			index.Manifests = append(index.Manifests[:i], index.Manifests[i+1:]...)
			if err := handler.updateIndex(config.HostCacheDir, index); err != nil {
				return err
			}
		}
	}
	return nil
}

func (handler *OciImageHandler) copyImage(srcRef types.ImageReference, destRef types.ImageReference, policyContext *signature.PolicyContext, cacheDir string) error {
	ctx := context.TODO()
	err := copy.Image(ctx, policyContext, destRef, srcRef, &copy.Options{
		RemoveSignatures: false,
		SignBy:           "",
		ReportWriter:     nil,
		SourceCtx:        handler.getSystemContext(cacheDir),
		DestinationCtx:   handler.getSystemContext(cacheDir),
	})
	if err != nil {
		return err
	}

	return nil
}

func (handler *OciImageHandler) getOutputWriter(config *ImageCacheConfig) io.Writer {
	var w io.Writer
	if config.Out != nil {
		w = config.Out
	} else {
		w = os.Stdout
	}
	return w
}

func (handler *OciImageHandler) getSystemContext(cacheDir string) *types.SystemContext {
	return &types.SystemContext{
		DockerDaemonHost:                  handler.dockerClientSettings.DockerHost,
		DockerDaemonCertPath:              handler.dockerClientSettings.DockerCertPath,
		DockerDaemonInsecureSkipTLSVerify: !handler.dockerClientSettings.DockerTLSVerify,
		OSChoice:                          "linux",
		ArchitectureChoice:                "amd64",
		OCIAcceptUncompressedLayers:       true,
		OCISharedBlobDirPath:              filepath.Join(cacheDir, "blobs"),
	}
}

// createSSHSession creates an interactive SSH session
func (handler *OciImageHandler) createSSHSession() (*ssh.Session, error) {
	sshClient, err := sshutil.NewSSHClient(handler.driver)
	if err != nil {
		return nil, err
	}

	session, err := sshClient.NewSession()
	if err != nil {
		return nil, err
	}

	return session, nil
}

func (handler *OciImageHandler) getPolicyContext() (*signature.PolicyContext, error) {
	policy := &signature.Policy{Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()}}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("Error creating security context: %s", err.Error()))
	}

	return policyContext, nil
}

func (handler *OciImageHandler) endProgress(progressDots *progressdots.ProgressDots, out io.Writer, status ProgressStatus) {
	progressDots.Stop()
	fmt.Fprintf(out, " %s\n", status.String())
}

func getDockerSettings(dockerEnv map[string]string) (*dockerClientConfig, error) {
	settings := &dockerClientConfig{}

	if val, ok := dockerEnv["DOCKER_HOST"]; ok {
		settings.DockerHost = val
	} else {
		return nil, errors.New("The provided Docker environment settings are missing the DOCKER_HOST key.")
	}

	if val, ok := dockerEnv["DOCKER_CERT_PATH"]; ok {
		settings.DockerCertPath = val
	} else {
		return nil, errors.New("The provided Docker environment settings are missing the DOCKER_CERT_PATH key.")
	}

	if val, ok := dockerEnv["DOCKER_TLS_VERIFY"]; ok {
		verify, err := strconv.ParseBool(val)
		if err != nil {
			return nil, errors.New(fmt.Sprintf("Invalid value '%s' for DOCKER_TLS_VERIFY key.", val))
		}
		settings.DockerTLSVerify = verify
	} else {
		return nil, errors.New("The provided Docker environment settings are missing the DOCKER_TLS_VERIFY key.")
	}

	return settings, nil
}

func (handler *OciImageHandler) progressStatusForError(err error) ProgressStatus {
	if err != nil {
		return FAIL
	}
	return OK
}

func (handler *OciImageHandler) updateIndex(cacheDir string, index *Index) error {
	indexPath := filepath.Join(cacheDir, "index.json")
	jsonData, err := json.MarshalIndent(index, "", "\t")
	if err != nil {
		return err
	}
	if err = ioutil.WriteFile(indexPath, jsonData, 0644); err != nil {
		return err
	}
	return nil
}
