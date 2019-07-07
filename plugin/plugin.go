package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/drone/drone-go/drone"
	"github.com/drone/drone-go/plugin/config"
	"github.com/drone/go-scm/scm"
	"github.com/drone/go-scm/scm/driver/github"
	"github.com/drone/go-scm/scm/transport"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

// New creates a drone plugin
func New(server, token string, concat bool, fallback bool, maxDepth int) config.Plugin {
	return &plugin{
		server: server,
		token:  token,
		concat: concat,
		fallback: fallback,
		maxDepth: maxDepth,
	}
}

type (
	plugin struct {
		server   string
		token    string
		concat   bool
		fallback bool
		maxDepth int
	}

	droneConfig struct {
		Name string `yaml:"name"`
		Kind string `yaml:"kind"`
	}

	request struct {
		*config.Request
		UUID   uuid.UUID
		Client *scm.Client
	}
)

var dedupRegex = regexp.MustCompile(`(?ms)(---[\s]*){2,}`)

// Find is called by drone
func (p *plugin) Find(ctx context.Context, droneRequest *config.Request) (*drone.Config, error) {
	requestUuid := uuid.New()
	logrus.Infof("%s %s/%s started", requestUuid, droneRequest.Repo.Namespace, droneRequest.Repo.Name)
	defer logrus.Infof("%s finished", requestUuid)

	// connect to SCM
	var client *scm.Client
	if p.server == "" {
		client = github.NewDefault()
	} else {
		var err error
		client, err = github.New(p.server)
		if err != nil {
			logrus.Errorf("%s Unable to connect to SCM: '%v'", requestUuid, err)
			return nil, err
		}
	}

	client.Client = &http.Client{
		Transport: &transport.BearerToken{
			Token: p.token,
		},
	}

	req := request{droneRequest, requestUuid, client}

	// get changed files
	changedFiles, err := p.getScmChanges(ctx, &req)
	if err != nil {
		return nil, err
	}

	// get drone.yml for changed files or all of them if no changes/cron
	configData := ""
	if changedFiles != nil {
		configData, err = p.getScmConfigData(ctx, &req, changedFiles)
	} else if req.Build.Trigger == "@cron" {
		logrus.Warnf("%s @cron, rebuilding all", req.UUID)
		configData, err = p.getAllConfigData(ctx, &req, "/", 0)
	} else if p.fallback {
		logrus.Warnf("%s no changed files and fallback enabled, rebuilding all", req.UUID)
		configData, err = p.getAllConfigData(ctx, &req, "/", 0)
	}
	if err != nil {
		return nil, err
	}

	// no file found
	if configData == "" {
		return nil, errors.New("did not find a .drone.yml")
	}

	// cleanup
	configData = strings.ReplaceAll(configData, "...", "")
	configData = string(dedupRegex.ReplaceAll([]byte(configData), []byte("---")))

	return &drone.Config{Data: configData}, nil
}

// getScmChanges tries to get a list of changed files from scm
func (p *plugin) getScmChanges(ctx context.Context, req *request) ([]string, error) {
	var changedFiles []string

	if req.Build.Trigger == "@cron" {
		// cron jobs trigger a full build
		changedFiles = []string{}
	} else if strings.HasPrefix(req.Build.Ref, "refs/pull/") {
		// use pullrequests api to get changed files
		pullRequestID, err := strconv.Atoi(strings.Split(req.Build.Ref, "/")[2])
		if err != nil {
			logrus.Errorf("%s unable to get pull request id %v", req.UUID, err)
			return nil, err
		}
		opts := scm.ListOptions{}
		files, _, err := req.Client.PullRequests.ListChanges(ctx, req.Repo.Slug, pullRequestID, opts)
		if err != nil {
			logrus.Errorf("%s unable to fetch diff for Pull request %v", req.UUID, err)
			return nil, err
		}
		for _, file := range files {
			changedFiles = append(changedFiles, file.Path)
		}
	} else {
		// use diff to get changed files
		before := req.Build.Before
		if before == "0000000000000000000000000000000000000000" || before == "" {
			before = fmt.Sprintf("%s~1", req.Build.After)
		}
		opts := scm.ListOptions{}
		// TODO verify that ListChanges is functionally equivalent to the /compare API
		changes, _, err := req.Client.Git.ListChanges(ctx, req.Repo.Slug, req.Build.After, opts)
		if err != nil {
			logrus.Errorf("%s unable to fetch diff: '%v'", req.UUID, err)
			return nil, err
		}
		for _, file := range changes {
			changedFiles = append(changedFiles, file.Path)
		}
	}

	if len(changedFiles) > 0 {
		changedList := strings.Join(changedFiles, "\n  ")
		logrus.Debugf("%s changed files: \n  %s", req.UUID, changedList)
	} else {
		return nil, nil
	}
	return changedFiles, nil
}

// getScmFile downloads a file from scm
func (p *plugin) getScmFile(ctx context.Context, req *request, file string) (content string, err error) {
	logrus.Debugf("%s checking %s/%s %s", req.UUID, req.Repo.Namespace, req.Repo.Name, file)

	data, _, err := req.Client.Contents.Find(ctx, req.Repo.Slug, file, req.Build.After)
	if data == nil {
		err = fmt.Errorf("failed to get %s: is not a file", file)
	}
	if err != nil {
		return "", err
	}
	return string(data.Data), nil
}

// getScmDroneConfig downloads a drone config and validates it
func (p *plugin) getScmDroneConfig(ctx context.Context, req *request, file string) (configData string, critical bool, err error) {
	fileContent, err := p.getScmFile(ctx, req, file)
	if err != nil {
		logrus.Debugf("%s skipping: unable to load file: %s %v", req.UUID, file, err)
		return "", false, err
	}

	// validate fileContent, exit early if an error was found
	dc := droneConfig{}
	err = yaml.Unmarshal([]byte(fileContent), &dc)
	if err != nil {
		logrus.Errorf("%s skipping: unable do parse yml file: %s %v", req.UUID, file, err)
		return "", true, err
	}
	if dc.Name == "" || dc.Kind == "" {
		logrus.Errorf("%s skipping: missing 'kind' or 'name' in %s.", req.UUID, file)
		return "", true, err
	}

	logrus.Infof("%s found %s/%s %s", req.UUID, req.Repo.Namespace, req.Repo.Name, file)
	return fileContent, false, nil
}

// getScmConfigData scans a repository based on the changed files
func (p *plugin) getScmConfigData(ctx context.Context, req *request, changedFiles []string) (configData string, err error) {
	// collect drone.yml files
	configData = ""
	cache := map[string]bool{}
	for _, file := range changedFiles {
		if !strings.HasPrefix(file, "/") {
			file = "/" + file
		}

		done := false
		dir := file
		for !done {
			done = bool(dir == "/")
			dir = path.Join(dir, "..")
			file := path.Join(dir, req.Repo.Config)

			// check if file has already been checked
			_, ok := cache[file]
			if ok {
				continue
			} else {
				cache[file] = true
			}

			// download file from git
			fileContent, critical, err := p.getScmDroneConfig(ctx, req, file)
			if err != nil {
				if critical {
					return "", err
				}
				continue
			}

			// append
			configData = p.droneConfigAppend(configData, fileContent)
			if !p.concat {
				logrus.Infof("%s concat is disabled. Using just first .drone.yml.", req.UUID)
				break
			}
		}
	}
	return configData, nil
}

// getAllConfigData searches for all or fist 'drone.yml' in the repo
func (p *plugin) getAllConfigData(ctx context.Context, req *request, dir string, depth int) (configData string, err error) {
	ls, _, err := req.Client.Contents.Find(ctx, req.Repo.Slug, dir, req.Build.After)
	if err != nil {
		return "", err
	}

	if depth > p.maxDepth {
		logrus.Infof("%s skipping scan of %s, max depth %d reached.", req.UUID, dir, depth)
		return "", nil
	}
	depth += 1

	// check recursivly for drone.yml
	configData = ""

	// TODO this will always crash because go-scm cannot handle a /contents request on a directory
	err2 := errors.New(string(ls.Data))
	//for _, f := range ls.Data {
	//	var fileContent string
	//	if f. == "dir" {
	//		fileContent, _ = p.getAllConfigData(ctx, req, *f.Path, depth)
	//	} else if *f.Type == "file" && *f.Name == req.Repo.Config {
	//		var critical bool
	//		fileContent, critical, err = p.getScmDroneConfig(ctx, req, *f.Path)
	//		if critical {
	//			return "", err
	//		}
	//	}
	//	// append
	//	configData = p.droneConfigAppend(configData, fileContent)
	//	if !p.concat {
	//		logrus.Infof("%s concat is disabled. Using just first .drone.yml.", req.UUID)
	//		break
	//	}
	//}

	return configData, err2

}

// droneConfigAppend concats multiple 'drone.yml's to a multi-machine pipeline
// see https://docs.drone.io/user-guide/pipeline/multi-machine/
func (p *plugin) droneConfigAppend(droneConfig string, appends ...string) string {
	for _, a := range appends {
		a = strings.Trim(a, " \n")
		if a != "" {
			if !strings.HasPrefix(a, "---\n") {
				a = "---\n" + a
			}
			droneConfig += a
			if !strings.HasSuffix(droneConfig, "\n") {
				droneConfig += "\n"
			}
		}
	}
	return droneConfig
}
