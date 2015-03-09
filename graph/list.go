package graph

import (
	"log"
	"path"
	"strings"

	"github.com/docker/docker/engine"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/parsers/filters"
	"github.com/docker/docker/utils"
)

var acceptedImageFilterTags = map[string]struct{}{"dangling": {}}

func (s *TagStore) CmdImages(job *engine.Job) engine.Status {
	var (
		allImages   map[string]*image.Image
		err         error
		filt_tagged = true
	)

	imageFilters, err := filters.FromParam(job.Getenv("filters"))
	if err != nil {
		return job.Error(err)
	}
	for name := range imageFilters {
		if _, ok := acceptedImageFilterTags[name]; !ok {
			return job.Errorf("Invalid filter '%s'", name)
		}
	}

	if i, ok := imageFilters["dangling"]; ok {
		for _, value := range i {
			if strings.ToLower(value) == "true" {
				filt_tagged = false
			}
		}
	}

	if job.GetenvBool("all") && filt_tagged {
		allImages, err = s.graph.Map()
	} else {
		allImages, err = s.graph.Heads()
	}
	if err != nil {
		return job.Error(err)
	}
	lookup := make(map[string]*engine.Env)
	s.Lock()
	for name, repository := range s.Repositories {
		if job.Getenv("filter") != "" {
			if match, _ := path.Match(job.Getenv("filter"), name); !match {
				continue
			}
		}
		for tag, id := range repository {
			image, err := s.graph.Get(id)
			if err != nil {
				log.Printf("Warning: couldn't load %s from %s/%s: %s", id, name, tag, err)
				continue
			}

			if out, exists := lookup[id]; exists {
				if filt_tagged {
					out.SetList("RepoTags", append(out.GetList("RepoTags"), utils.ImageReference(name, tag)))
				}
			} else {
				// get the boolean list for if only the untagged images are requested
				delete(allImages, id)
				if filt_tagged {
					out := &engine.Env{}
					out.SetJson("ParentId", image.Parent)
					out.SetList("RepoTags", []string{utils.ImageReference(name, tag)})
					out.SetJson("Id", image.ID)
					out.SetInt64("Created", image.Created.Unix())
					out.SetInt64("Size", image.Size)
					out.SetInt64("VirtualSize", image.GetParentsSize(0)+image.Size)
					lookup[id] = out
				}
			}

		}
	}
	s.Unlock()

	showDigests := job.GetenvBool("digests")
	for name, repository := range s.Digests {
		if job.Getenv("filter") != "" {
			if match, _ := path.Match(job.Getenv("filter"), name); !match {
				continue
			}
		}
		for digest, mapping := range repository {
			id := mapping.V1ImageID
			image, err := s.graph.Get(id)
			if err != nil {
				log.Printf("Warning: couldn't load %s from %s@%s: %s", id, name, digest, err)
				continue
			}

			// remove from allImages so it doesn't show up as dangling
			delete(allImages, id)

			repoDigestsKey := utils.ImageReference(name, mapping.Tag)
			if showDigests && filt_tagged {
				if out, exists := lookup[id]; exists {
					repoDigests := make(map[string]string)
					out.GetJson("RepoDigests", &repoDigests)
					repoDigests[repoDigestsKey] = digest
					out.SetJson("RepoDigests", repoDigests)
				} else {
					out := &engine.Env{}
					out.SetJson("ParentId", image.Parent)
					out.SetJson("RepoDigests", map[string]string{repoDigestsKey: digest})
					out.SetJson("Id", image.ID)
					out.SetInt64("Created", image.Created.Unix())
					out.SetInt64("Size", image.Size)
					out.SetInt64("VirtualSize", image.GetParentsSize(0)+image.Size)
					lookup[id] = out
				}
			}
		}
	}

	outs := engine.NewTable("Created", len(lookup))
	for _, value := range lookup {
		outs.Add(value)
	}

	// Display images which aren't part of a repository/tag
	if job.Getenv("filter") == "" {
		for _, image := range allImages {
			out := &engine.Env{}
			out.SetJson("ParentId", image.Parent)
			out.SetList("RepoTags", []string{"<none>:<none>"})
			out.SetJson("Id", image.ID)
			out.SetInt64("Created", image.Created.Unix())
			out.SetInt64("Size", image.Size)
			out.SetInt64("VirtualSize", image.GetParentsSize(0)+image.Size)
			outs.Add(out)
		}
	}

	outs.ReverseSort()
	if _, err := outs.WriteListTo(job.Stdout); err != nil {
		return job.Error(err)
	}
	return engine.StatusOK
}
