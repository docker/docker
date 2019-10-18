package images // import "github.com/docker/docker/daemon/images"

import (
	"context"

	"github.com/docker/docker/api/types/events"
)

// LogImageEvent generates an event related to an image with only the default attributes.
func (i *ImageService) LogImageEvent(ctx context.Context, imageID, refName, action string) {
	if i.eventsService == nil {
		return
	}

	// image has not been removed yet.
	// it could be missing if the event is `delete`.
	attributes, _ := i.getImageLabels(ctx, imageID)

	if attributes == nil {
		attributes = map[string]string{}
	}

	if refName != "" {
		attributes["name"] = refName
	}
	actor := events.Actor{
		ID:         imageID,
		Attributes: attributes,
	}

	i.eventsService.Log(action, events.ImageEventType, actor)
}

func (i *ImageService) getImageLabels(ctx context.Context, imageID string) (map[string]string, error) {
	return nil, nil
	// TODO(containerd): why is this expensive operation necessary
	// would require resolving imageID to manifest, then reading
	// and unmarshalling the config, this would also require
	// resolving manifest list if imageID is a manifest list
}
