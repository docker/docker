package docker

import (
	"encoding/json"
	"fmt"
	"github.com/dotcloud/docker/auth"
	"github.com/dotcloud/docker/utils"
	"github.com/gorilla/mux"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const APIVERSION = 1.2

func hijackServer(w http.ResponseWriter) (io.ReadCloser, io.Writer, error) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	// Flush the options to make sure the client sets the raw mode
	conn.Write([]byte{})
	return conn, conn, nil
}

//If we don't do this, POST method without Content-type (even with empty body) will fail
func parseForm(r *http.Request) error {
	if err := r.ParseForm(); err != nil && !strings.HasPrefix(err.Error(), "mime:") {
		return err
	}
	return nil
}

func parseMultipartForm(r *http.Request) error {
	if err := r.ParseMultipartForm(4096); err != nil && !strings.HasPrefix(err.Error(), "mime:") {
		return err
	}
	return nil
}

func httpError(w http.ResponseWriter, err error) {
	if strings.HasPrefix(err.Error(), "No such") {
		http.Error(w, err.Error(), http.StatusNotFound)
	} else if strings.HasPrefix(err.Error(), "Bad parameter") {
		http.Error(w, err.Error(), http.StatusBadRequest)
	} else if strings.HasPrefix(err.Error(), "Conflict") {
		http.Error(w, err.Error(), http.StatusConflict)
	} else if strings.HasPrefix(err.Error(), "Impossible") {
		http.Error(w, err.Error(), http.StatusNotAcceptable)
	} else {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// FIXME: Use stvconv.ParseBool() instead?
func getBoolParam(value string) (bool, error) {
	if value == "1" || strings.ToLower(value) == "true" {
		return true, nil
	}
	if value == "" || value == "0" || strings.ToLower(value) == "false" {
		return false, nil
	}
	return false, fmt.Errorf("Bad parameter")
}

func getAuth(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if version > 1.1 {
		w.WriteHeader(http.StatusNotFound)
		return nil
	}
	authConfig, err := auth.LoadConfig(srv.GetRuntime().root)
	if err != nil {
		if err != auth.ErrConfigFileMissing {
			return err
		}
		authConfig = &auth.AuthConfig{}
	}
	b, err := json.Marshal(&auth.AuthConfig{Username: authConfig.Username, Email: authConfig.Email})
	if err != nil {
		return err
	}
	writeJSON(w, b)
	return nil
}

func postAuth(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	authConfig := &auth.AuthConfig{}
	err := json.NewDecoder(r.Body).Decode(authConfig)
	if err != nil {
		return err
	}
	status := ""
	if version > 1.1 {
		status, err = auth.Login(authConfig, false)
		if err != nil {
			return err
		}
	} else {
		localAuthConfig, err := auth.LoadConfig(srv.GetRuntime().root)
		if err != nil {
			if err != auth.ErrConfigFileMissing {
				return err
			}
		}
		if authConfig.Username == localAuthConfig.Username {
			authConfig.Password = localAuthConfig.Password
		}

		newAuthConfig := auth.NewAuthConfig(authConfig.Username, authConfig.Password, authConfig.Email, srv.GetRuntime().root)
		status, err = auth.Login(newAuthConfig, true)
		if err != nil {
			return err
		}
	}
	if status != "" {
		b, err := json.Marshal(&APIAuth{Status: status})
		if err != nil {
			return err
		}
		writeJSON(w, b)
		return nil
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func getVersion(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	m := srv.DockerVersion()
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	writeJSON(w, b)
	return nil
}

func postContainersKill(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]
	if err := srv.ContainerKill(name); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func getContainersExport(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]

	if err := srv.ContainerExport(name, w); err != nil {
		utils.Debugf("%s", err.Error())
		return err
	}
	return nil
}

func getImagesJSON(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}

	all, err := getBoolParam(r.Form.Get("all"))
	if err != nil {
		return err
	}
	filter := r.Form.Get("filter")

	outs, err := srv.Images(all, filter)
	if err != nil {
		return err
	}
	b, err := json.Marshal(outs)
	if err != nil {
		return err
	}
	writeJSON(w, b)
	return nil
}

func getImagesViz(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := srv.ImagesViz(w); err != nil {
		return err
	}
	return nil
}

func getInfo(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	out := srv.DockerInfo()
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	writeJSON(w, b)
	return nil
}

func getImagesHistory(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]
	outs, err := srv.ImageHistory(name)
	if err != nil {
		return err
	}
	b, err := json.Marshal(outs)
	if err != nil {
		return err
	}
	writeJSON(w, b)
	return nil
}

func getContainersChanges(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]
	changesStr, err := srv.ContainerChanges(name)
	if err != nil {
		return err
	}
	b, err := json.Marshal(changesStr)
	if err != nil {
		return err
	}
	writeJSON(w, b)
	return nil
}

func getContainersJSON(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}
	all, err := getBoolParam(r.Form.Get("all"))
	if err != nil {
		return err
	}
	since := r.Form.Get("since")
	before := r.Form.Get("before")
	n, err := strconv.Atoi(r.Form.Get("limit"))
	if err != nil {
		n = -1
	}

	outs := srv.Containers(all, n, since, before)
	b, err := json.Marshal(outs)
	if err != nil {
		return err
	}
	writeJSON(w, b)
	return nil
}

func postImagesTag(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}
	repo := r.Form.Get("repo")
	tag := r.Form.Get("tag")
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]
	force, err := getBoolParam(r.Form.Get("force"))
	if err != nil {
		return err
	}

	if err := srv.ContainerTag(name, repo, tag, force); err != nil {
		return err
	}
	w.WriteHeader(http.StatusCreated)
	return nil
}

func postCommit(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}
	config := &Config{}
	if err := json.NewDecoder(r.Body).Decode(config); err != nil {
		utils.Debugf("%s", err.Error())
	}
	repo := r.Form.Get("repo")
	tag := r.Form.Get("tag")
	container := r.Form.Get("container")
	author := r.Form.Get("author")
	comment := r.Form.Get("comment")
	id, err := srv.ContainerCommit(container, repo, tag, author, comment, config)
	if err != nil {
		return err
	}
	b, err := json.Marshal(&APIID{id})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, b)
	return nil
}

// Creates an image from Pull or from Import
func postImagesCreate(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}

	src := r.Form.Get("fromSrc")
	image := r.Form.Get("fromImage")
	tag := r.Form.Get("tag")
	repo := r.Form.Get("repo")

	if version > 1.0 {
		w.Header().Set("Content-Type", "application/json")
	}
	sf := utils.NewStreamFormatter(version > 1.0)
	if image != "" { //pull
		registry := r.Form.Get("registry")
		if err := srv.ImagePull(image, tag, registry, w, sf, &auth.AuthConfig{}); err != nil {
			if sf.Used() {
				w.Write(sf.FormatError(err))
				return nil
			}
			return err
		}
	} else { //import
		if err := srv.ImageImport(src, repo, tag, r.Body, w, sf); err != nil {
			if sf.Used() {
				w.Write(sf.FormatError(err))
				return nil
			}
			return err
		}
	}
	return nil
}

func getImagesSearch(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}

	term := r.Form.Get("term")
	outs, err := srv.ImagesSearch(term)
	if err != nil {
		return err
	}
	b, err := json.Marshal(outs)
	if err != nil {
		return err
	}
	writeJSON(w, b)
	return nil
}

func postImagesInsert(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}

	url := r.Form.Get("url")
	path := r.Form.Get("path")
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]
	if version > 1.0 {
		w.Header().Set("Content-Type", "application/json")
	}
	sf := utils.NewStreamFormatter(version > 1.0)
	imgID, err := srv.ImageInsert(name, url, path, w, sf)
	if err != nil {
		if sf.Used() {
			w.Write(sf.FormatError(err))
			return nil
		}
	}
	b, err := json.Marshal(&APIID{ID: imgID})
	if err != nil {
		return err
	}
	writeJSON(w, b)
	return nil
}

func postImagesPush(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	authConfig := &auth.AuthConfig{}
	if version > 1.1 {
		if err := json.NewDecoder(r.Body).Decode(authConfig); err != nil {
			return err
		}
	} else {
		localAuthConfig, err := auth.LoadConfig(srv.GetRuntime().root)
		if err != nil && err != auth.ErrConfigFileMissing {
			return err
		}
		authConfig = localAuthConfig
	}
	if err := parseForm(r); err != nil {
		return err
	}
	registry := r.Form.Get("registry")

	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]
	if version > 1.0 {
		w.Header().Set("Content-Type", "application/json")
	}
	sf := utils.NewStreamFormatter(version > 1.0)
	if err := srv.ImagePush(name, registry, w, sf, authConfig); err != nil {
		if sf.Used() {
			w.Write(sf.FormatError(err))
			return nil
		}
		return err
	}
	return nil
}

func postContainersCreate(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	config := &Config{}
	out := &APIRun{}

	if err := json.NewDecoder(r.Body).Decode(config); err != nil {
		return err
	}

	if len(config.Dns) == 0 && len(srv.GetRuntime().Dns) == 0 && utils.CheckLocalDns() {
		out.Warnings = append(out.Warnings, fmt.Sprintf("Docker detected local DNS server on resolv.conf. Using default external servers: %v", defaultDns))
		config.Dns = defaultDns
	}

	id, err := srv.ContainerCreate(config)
	if err != nil {
		return err
	}
	out.ID = id

	if config.Memory > 0 && !srv.GetRuntime().capabilities.MemoryLimit {
		log.Println("WARNING: Your kernel does not support memory limit capabilities. Limitation discarded.")
		out.Warnings = append(out.Warnings, "Your kernel does not support memory limit capabilities. Limitation discarded.")
	}
	if config.Memory > 0 && !srv.GetRuntime().capabilities.SwapLimit {
		log.Println("WARNING: Your kernel does not support swap limit capabilities. Limitation discarded.")
		out.Warnings = append(out.Warnings, "Your kernel does not support memory swap capabilities. Limitation discarded.")
	}

	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, b)
	return nil
}

func postContainersRestart(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}
	t, err := strconv.Atoi(r.Form.Get("t"))
	if err != nil || t < 0 {
		t = 10
	}
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]
	if err := srv.ContainerRestart(name, t); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func deleteContainers(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]
	removeVolume, err := getBoolParam(r.Form.Get("v"))
	if err != nil {
		return err
	}

	if err := srv.ContainerDestroy(name, removeVolume); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func deleteImages(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]
	imgs, err := srv.ImageDelete(name, version > 1.1)
	if err != nil {
		return err
	}
	if imgs != nil {
		if len(*imgs) != 0 {
			b, err := json.Marshal(imgs)
			if err != nil {
				return err
			}
			writeJSON(w, b)
		} else {
			return fmt.Errorf("Conflict, %s wasn't deleted", name)
		}
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
	return nil
}

func postContainersStart(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]
	if err := srv.ContainerStart(name); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func postContainersStop(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}
	t, err := strconv.Atoi(r.Form.Get("t"))
	if err != nil || t < 0 {
		t = 10
	}

	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]

	if err := srv.ContainerStop(name, t); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func postContainersWait(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]
	status, err := srv.ContainerWait(name)
	if err != nil {
		return err
	}
	b, err := json.Marshal(&APIWait{StatusCode: status})
	if err != nil {
		return err
	}
	writeJSON(w, b)
	return nil
}

func postContainersResize(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}
	height, err := strconv.Atoi(r.Form.Get("h"))
	if err != nil {
		return err
	}
	width, err := strconv.Atoi(r.Form.Get("w"))
	if err != nil {
		return err
	}
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]
	if err := srv.ContainerResize(name, height, width); err != nil {
		return err
	}
	return nil
}

func postContainersAttach(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := parseForm(r); err != nil {
		return err
	}
	logs, err := getBoolParam(r.Form.Get("logs"))
	if err != nil {
		return err
	}
	stream, err := getBoolParam(r.Form.Get("stream"))
	if err != nil {
		return err
	}
	stdin, err := getBoolParam(r.Form.Get("stdin"))
	if err != nil {
		return err
	}
	stdout, err := getBoolParam(r.Form.Get("stdout"))
	if err != nil {
		return err
	}
	stderr, err := getBoolParam(r.Form.Get("stderr"))
	if err != nil {
		return err
	}

	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]

	if _, err := srv.ContainerInspect(name); err != nil {
		return err
	}

	in, out, err := hijackServer(w)
	if err != nil {
		return err
	}
	defer in.Close()

	fmt.Fprintf(out, "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n")
	if err := srv.ContainerAttach(name, logs, stream, stdin, stdout, stderr, in, out); err != nil {
		fmt.Fprintf(out, "Error: %s\n", err)
	}
	return nil
}

func getContainersByName(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]

	container, err := srv.ContainerInspect(name)
	if err != nil {
		return err
	}
	b, err := json.Marshal(container)
	if err != nil {
		return err
	}
	writeJSON(w, b)
	return nil
}

func getImagesByName(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	name := vars["name"]

	image, err := srv.ImageInspect(name)
	if err != nil {
		return err
	}
	b, err := json.Marshal(image)
	if err != nil {
		return err
	}
	writeJSON(w, b)
	return nil
}

func postImagesGetCache(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	apiConfig := &APIImageConfig{}
	if err := json.NewDecoder(r.Body).Decode(apiConfig); err != nil {
		return err
	}

	image, err := srv.ImageGetCached(apiConfig.ID, apiConfig.Config)
	if err != nil {
		return err
	}
	if image == nil {
		w.WriteHeader(http.StatusNotFound)
		return nil
	}
	apiID := &APIID{ID: image.ID}
	b, err := json.Marshal(apiID)
	if err != nil {
		return err
	}
	writeJSON(w, b)
	return nil
}

func postBuild(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := r.ParseMultipartForm(4096); err != nil {
		return err
	}
	remote := r.FormValue("t")
	tag := ""
	if strings.Contains(remote, ":") {
		remoteParts := strings.Split(remote, ":")
		tag = remoteParts[1]
		remote = remoteParts[0]
	}

	dockerfile, _, err := r.FormFile("Dockerfile")
	if err != nil {
		return err
	}

	context, _, err := r.FormFile("Context")
	if err != nil {
		if err != http.ErrMissingFile {
			return err
		}
	}

	b := NewBuildFile(srv, utils.NewWriteFlusher(w))
	if id, err := b.Build(dockerfile, context); err != nil {
		fmt.Fprintf(w, "Error build: %s\n", err)
	} else if remote != "" {
		srv.GetRuntime().repositories.Set(remote, tag, id, false)
	}
	return nil
}

func postClean(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	err := parseForm(r)
	if err != nil {
		return err
	}

	removeVolumes := r.Form.Get("removeVolumes")
	removeVolumesBool := true

	if removeVolumes != "" {
		removeVolumesBool, err = strconv.ParseBool(removeVolumes)
		if err != nil {
			return err
		}
	}

	lastStartedTime, lastStarted, err := getTimeFromArg("lastStarted", r)
	if err != nil {
		return err
	}

	createdBeforeTime, createdBefore, err := getTimeFromArg("createdBefore", r)
	if err != nil {
		return err
	}

	durationLessThan, durationLessThanArg, err := getDurationFromArg("durationLessThan", r)
	if err != nil {
		return err
	}

	for _, container := range srv.GetRuntime().List() {

		shouldDestroy := true
		if createdBefore != "" && container.Created.After(createdBeforeTime) {
			shouldDestroy = false
		}
		if lastStarted != "" && container.State.StartedAt.After(lastStartedTime) {
			shouldDestroy = false
		}
		if durationLessThanArg != "" && durationLessThan < container.State.Uptime() {
			shouldDestroy = false
		}

		if shouldDestroy {
			srv.ContainerDestroy(container.ID, removeVolumesBool)
		}
	}
	return nil
}

func getDurationFromArg(someKey string, r *http.Request) (time.Duration, string, error) {
	some := r.Form.Get(someKey)
	if some == "" {
		return time.Duration(0), some, nil
	}
	theDuration, err := time.ParseDuration(some)
	if err != nil {
		return time.Duration(0), some, err
	}
	return theDuration, some, nil
}

// utility to get time.Time from a form
func getTimeFromArg(someKey string, r *http.Request) (time.Time, string, error) {
	var beginningOfTime time.Time
	some := r.Form.Get(someKey)
	if some == "" {
		return beginningOfTime, some, nil
	}
	someInt, err := strconv.ParseInt(some, 10, 64)
	if err != nil {
		return beginningOfTime, some, err
	}
	return time.Unix(someInt, 0), some, nil
}

func optionsHandler(srv Server, version float64, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	w.WriteHeader(http.StatusOK)
	return nil
}
func writeCorsHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Access-Control-Allow-Origin", "*")
	w.Header().Add("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")
	w.Header().Add("Access-Control-Allow-Methods", "GET, POST, DELETE, PUT, OPTIONS")
}

func createRouter(srv Server, logging bool) (*mux.Router, error) {
	r := mux.NewRouter()

	m := map[string]map[string]func(Server, float64, http.ResponseWriter, *http.Request, map[string]string) error{
		"GET": {
			"/auth":                         getAuth,
			"/version":                      getVersion,
			"/info":                         getInfo,
			"/images/json":                  getImagesJSON,
			"/images/viz":                   getImagesViz,
			"/images/search":                getImagesSearch,
			"/images/{name:.*}/history":     getImagesHistory,
			"/images/{name:.*}/json":        getImagesByName,
			"/containers/ps":                getContainersJSON,
			"/containers/json":              getContainersJSON,
			"/containers/{name:.*}/export":  getContainersExport,
			"/containers/{name:.*}/changes": getContainersChanges,
			"/containers/{name:.*}/json":    getContainersByName,
		},
		"POST": {
			"/auth":                         postAuth,
			"/commit":                       postCommit,
			"/build":                        postBuild,
			"/clean":                        postClean,
			"/images/create":                postImagesCreate,
			"/images/{name:.*}/insert":      postImagesInsert,
			"/images/{name:.*}/push":        postImagesPush,
			"/images/{name:.*}/tag":         postImagesTag,
			"/images/getCache":              postImagesGetCache,
			"/containers/create":            postContainersCreate,
			"/containers/{name:.*}/kill":    postContainersKill,
			"/containers/{name:.*}/restart": postContainersRestart,
			"/containers/{name:.*}/start":   postContainersStart,
			"/containers/{name:.*}/stop":    postContainersStop,
			"/containers/{name:.*}/wait":    postContainersWait,
			"/containers/{name:.*}/resize":  postContainersResize,
			"/containers/{name:.*}/attach":  postContainersAttach,
		},
		"DELETE": {
			"/containers/{name:.*}": deleteContainers,
			"/images/{name:.*}":     deleteImages,
		},
		"OPTIONS": {
			"": optionsHandler,
		},
	}

	for method, routes := range m {
		for route, fct := range routes {
			utils.Debugf("Registering %s, %s", method, route)
			// NOTE: scope issue, make sure the variables are local and won't be changed
			localRoute := route
			localMethod := method
			localFct := fct
			f := func(w http.ResponseWriter, r *http.Request) {
				utils.Debugf("Calling %s %s", localMethod, localRoute)
				if logging {
					log.Println(r.Method, r.RequestURI)
				}
				if strings.Contains(r.Header.Get("User-Agent"), "Docker-Client/") {
					userAgent := strings.Split(r.Header.Get("User-Agent"), "/")
					if len(userAgent) == 2 && userAgent[1] != VERSION {
						utils.Debugf("Warning: client and server don't have the same version (client: %s, server: %s)", userAgent[1], VERSION)
					}
				}
				version, err := strconv.ParseFloat(mux.Vars(r)["version"], 64)
				if err != nil {
					version = APIVERSION
				}
				if srv.GetEnableCors() {
					writeCorsHeaders(w, r)
				}
				if version == 0 || version > APIVERSION {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if err := localFct(srv, version, w, r, mux.Vars(r)); err != nil {
					httpError(w, err)
				}
			}

			if localRoute == "" {
				r.Methods(localMethod).HandlerFunc(f)
			} else {
				r.Path("/v{version:[0-9.]+}" + localRoute).Methods(localMethod).HandlerFunc(f)
				r.Path(localRoute).Methods(localMethod).HandlerFunc(f)
			}
		}
	}
	return r, nil
}

func ListenAndServe(addr string, srv Server, logging bool) error {
	log.Printf("Listening for HTTP on %s\n", addr)

	r, err := createRouter(srv, logging)
	if err != nil {
		return err
	}
	return http.ListenAndServe(addr, r)
}
