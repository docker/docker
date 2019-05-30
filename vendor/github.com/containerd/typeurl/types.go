/*
   Copyright The containerd Authors.

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

package typeurl

import (
	"encoding/json"
	"path"
	"reflect"
	"sync"

	"github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"
	"github.com/pkg/errors"
)

var (
	mu       sync.Mutex
	registry = make(map[reflect.Type]string)
)

var ErrNotFound = errors.New("not found")

// Register a type with the base url of the type
func Register(v interface{}, args ...string) {
	var (
		t = tryDereference(v)
		p = path.Join(args...)
	)
	mu.Lock()
	defer mu.Unlock()
	if et, ok := registry[t]; ok {
		if et != p {
			panic(errors.Errorf("type registred with alternate path %q != %q", et, p))
		}
		return
	}
	registry[t] = p
}

// TypeURL returns the type url for a registred type
func TypeURL(v interface{}) (string, error) {
	mu.Lock()
	u, ok := registry[tryDereference(v)]
	mu.Unlock()
	if !ok {
		// fallback to the proto registry if it is a proto message
		pb, ok := v.(proto.Message)
		if !ok {
			return "", errors.Wrapf(ErrNotFound, "type %s", reflect.TypeOf(v))
		}
		return proto.MessageName(pb), nil
	}
	return u, nil
}

// Is returns true if the type of the Any is the same as v
func Is(any *types.Any, v interface{}) bool {
	// call to check that v is a pointer
	tryDereference(v)
	url, err := TypeURL(v)
	if err != nil {
		return false
	}
	return any.TypeUrl == url
}

// MarshalAny marshals the value v into an any with the correct TypeUrl.
// If the provided object is already a proto.Any message, then it will be
// returned verbatim. If it is of type proto.Message, it will be marshaled as a
// protocol buffer. Otherwise, the object will be marshaled to json.
func MarshalAny(v interface{}) (*types.Any, error) {
	var marshal func(v interface{}) ([]byte, error)
	switch t := v.(type) {
	case *types.Any:
		// avoid reserializing the type if we have an any.
		return t, nil
	case proto.Message:
		marshal = func(v interface{}) ([]byte, error) {
			return proto.Marshal(t)
		}
	default:
		marshal = json.Marshal
	}

	url, err := TypeURL(v)
	if err != nil {
		return nil, err
	}

	data, err := marshal(v)
	if err != nil {
		return nil, err
	}
	return &types.Any{
		TypeUrl: url,
		Value:   data,
	}, nil
}

// UnmarshalAny unmarshals the any type into a concrete type
func UnmarshalAny(any *types.Any) (interface{}, error) {
	t, err := getTypeByUrl(any.TypeUrl)
	if err != nil {
		return nil, err
	}
	v := reflect.New(t.t).Interface()
	if t.isProto {
		err = proto.Unmarshal(any.Value, v.(proto.Message))
	} else {
		err = json.Unmarshal(any.Value, v)
	}
	return v, err
}

type urlType struct {
	t       reflect.Type
	isProto bool
}

func getTypeByUrl(url string) (urlType, error) {
	for t, u := range registry {
		if u == url {
			return urlType{
				t: t,
			}, nil
		}
	}
	// fallback to proto registry
	t := proto.MessageType(url)
	if t != nil {
		return urlType{
			// get the underlying Elem because proto returns a pointer to the type
			t:       t.Elem(),
			isProto: true,
		}, nil
	}
	return urlType{}, errors.Wrapf(ErrNotFound, "type with url %s", url)
}

func tryDereference(v interface{}) reflect.Type {
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Ptr {
		// require check of pointer but dereference to register
		return t.Elem()
	}
	panic("v is not a pointer to a type")
}
