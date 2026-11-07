package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/container"
)

// ContainerStatPathOptions holds options for [Client.ContainerStatPath].
type ContainerStatPathOptions struct {
	Path string
}

// ContainerStatPathResult holds the result of [Client.ContainerStatPath].
type ContainerStatPathResult struct {
	Stat container.PathStat
}

// ContainerStatPath returns stat information about a path inside the container filesystem.
func (cli *Client) ContainerStatPath(ctx context.Context, containerID string, options ContainerStatPathOptions) (ContainerStatPathResult, error) {
	containerID, err := trimID("container", containerID)
	if err != nil {
		return ContainerStatPathResult{}, err
	}

	query := url.Values{}
	query.Set("path", filepath.ToSlash(options.Path)) // Normalize the paths used in the API.

	resp, err := cli.head(ctx, "/containers/"+containerID+"/archive", query, nil)
	defer ensureReaderClosed(resp)
	if err != nil {
		return ContainerStatPathResult{}, err
	}
	stat, err := getContainerPathStatFromHeader(resp.Header)
	if err != nil {
		return ContainerStatPathResult{}, err
	}
	return ContainerStatPathResult{Stat: stat}, nil
}

// CopyToContainerOptions holds information about files to copy into a container.
type CopyToContainerOptions struct {
	// DestinationPath is the path inside the container where Content is extracted.
	DestinationPath string
	// Content is a tar archive stream to extract into DestinationPath.
	Content io.Reader

	AllowOverwriteDirWithFile bool
	CopyUIDGID                bool
}

// CopyToContainerResult holds the result of [Client.CopyToContainer].
type CopyToContainerResult struct{}

// CopyToContainer copies a tar archive into the container filesystem.
// The archive in options.Content is extracted to options.DestinationPath.
func (cli *Client) CopyToContainer(ctx context.Context, containerID string, options CopyToContainerOptions) (CopyToContainerResult, error) {
	containerID, err := trimID("container", containerID)
	if err != nil {
		return CopyToContainerResult{}, err
	}

	query := url.Values{}
	query.Set("path", filepath.ToSlash(options.DestinationPath)) // Normalize the paths used in the API.
	// Do not allow for an existing directory to be overwritten by a non-directory and vice versa.
	if !options.AllowOverwriteDirWithFile {
		query.Set("noOverwriteDirNonDir", "true")
	}

	if options.CopyUIDGID {
		query.Set("copyUIDGID", "true")
	}

	response, err := cli.putRaw(ctx, "/containers/"+containerID+"/archive", query, nil, options.Content)
	defer ensureReaderClosed(response)
	if err != nil {
		return CopyToContainerResult{}, err
	}

	return CopyToContainerResult{}, nil
}

// CopyFromContainerOptions holds options for [Client.CopyFromContainer].
type CopyFromContainerOptions struct {
	SourcePath string
}

// CopyFromContainerResult holds the result of [Client.CopyFromContainer].
type CopyFromContainerResult struct {
	Content io.ReadCloser
	Stat    container.PathStat
}

// CopyFromContainer gets the content from the container and returns it as a Reader
// for a TAR archive to manipulate it in the host. It's up to the caller to close the reader.
func (cli *Client) CopyFromContainer(ctx context.Context, containerID string, options CopyFromContainerOptions) (CopyFromContainerResult, error) {
	containerID, err := trimID("container", containerID)
	if err != nil {
		return CopyFromContainerResult{}, err
	}

	query := make(url.Values, 1)
	query.Set("path", filepath.ToSlash(options.SourcePath)) // Normalize the paths used in the API.

	resp, err := cli.get(ctx, "/containers/"+containerID+"/archive", query, nil)
	if err != nil {
		return CopyFromContainerResult{}, err
	}

	// In order to get the copy behavior right, we need to know information
	// about both the source and the destination. The response headers include
	// stat info about the source that we can use in deciding exactly how to
	// copy it locally. Along with the stat info about the local destination,
	// we have everything we need to handle the multiple possibilities there
	// can be when copying a file/dir from one location to another file/dir.
	stat, err := getContainerPathStatFromHeader(resp.Header)
	if err != nil {
		ensureReaderClosed(resp)
		return CopyFromContainerResult{Stat: stat}, fmt.Errorf("unable to get resource stat from response: %s", err)
	}
	return CopyFromContainerResult{Content: resp.Body, Stat: stat}, nil
}

func getContainerPathStatFromHeader(header http.Header) (container.PathStat, error) {
	var stat container.PathStat

	encodedStat := header.Get("X-Docker-Container-Path-Stat")
	statDecoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(encodedStat))

	err := json.NewDecoder(statDecoder).Decode(&stat)
	if err != nil {
		err = fmt.Errorf("unable to decode container path stat header: %s", err)
	}

	return stat, err
}
