// Copyright 2017 Istio Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package descriptor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/gogo/protobuf/proto"

	dpb "istio.io/api/mixer/v1/config/descriptor"
	"istio.io/istio/mixer/pkg/adapter"
	pb "istio.io/istio/mixer/pkg/config/proto"
	"istio.io/istio/mixer/pkg/expr"
	"istio.io/istio/pkg/log"
)

// Finder describes anything that can provide a view into the config's descriptors by name and type.
type Finder interface {
	expr.AttributeDescriptorFinder
}

type dname string

// NOTE: Update this list when new fields are added to the GlobalConfig message.
const (
	logs               dname = "logs"
	metrics                  = "metrics"
	monitoredResources       = "monitoredResources"
	principals               = "principals"
	quotas                   = "quotas"
	manifests                = "manifests"
	attributes               = "attributes"
)

type finder struct {
	attributes map[string]*pb.AttributeManifest_AttributeInfo
}

// typeMap maps descriptor types to example messages of those types.
var typeMap = map[dname]proto.Message{
	attributes: &pb.AttributeManifest_AttributeInfo{
		ValueType:   dpb.STRING,
		Description: "Intended destination of a request",
	},
	manifests: &pb.AttributeManifest{
		Attributes: map[string]*pb.AttributeManifest_AttributeInfo{
			"target.service": {
				ValueType:   dpb.STRING,
				Description: "Intended destination of a request",
			},
		},
	},
}

// Parse parses a descriptor config into its parts.
// This entire function is equivalent to jsonpb.Unmarshal()
// It provides better error reporting about which specific
// object has the error
func Parse(cfg string) (dcfg *pb.GlobalConfig, ce *adapter.ConfigErrors) {
	m := map[string]interface{}{}
	var err error
	if err = yaml.Unmarshal([]byte(cfg), &m); err != nil {
		return nil, ce.Append("descriptorConfig", err)
	}
	var cerr *adapter.ConfigErrors
	var oarr reflect.Value

	basemsg := map[string]interface{}{}
	// copy unhandled keys into basemsg
	for kk, v := range m {
		if typeMap[dname(kk)] == nil {
			basemsg[kk] = v
		}
	}

	dcfg = &pb.GlobalConfig{}

	ce = ce.Extend(updateMsg("descriptorConfig", basemsg, dcfg, nil, true))

	//flatten manifest
	var k dname = manifests
	val := m[string(k)]
	if val != nil {
		mani := []*pb.AttributeManifest{}
		for midx, msft := range val.([]interface{}) {
			mname := fmt.Sprintf("%s[%d]", k, midx)
			manifest := msft.(map[string]interface{})
			attr := manifest[attributes].(map[string]interface{})
			delete(manifest, attributes)
			ma := &pb.AttributeManifest{}
			if cerr = updateMsg(mname, manifest, ma, typeMap[k], false); cerr != nil {
				ce = ce.Extend(cerr)
			}
			if oarr, cerr = processMap(mname+"."+attributes, attr, typeMap[attributes]); cerr != nil {
				ce = ce.Extend(cerr)
				continue
			}
			ma.Attributes = oarr.Interface().(map[string]*pb.AttributeManifest_AttributeInfo)
			mani = append(mani, ma)
		}
		dcfg.Manifests = mani
	}

	for k, kfn := range typeMap {
		if k == manifests {
			continue
		}
		val = m[string(k)]
		if val == nil {
			log.Debugf("%s missing", k)
			continue
		}

		if oarr, cerr = processArray(string(k), val.([]interface{}), kfn); cerr != nil {
			ce = ce.Extend(cerr)
			continue
		}
		fld := reflect.ValueOf(dcfg).Elem().FieldByName(strings.Title(string(k)))
		if !fld.IsValid() {
			continue
		}
		fld.Set(oarr)
	}

	return dcfg, ce
}

// updateMsg updates a proto.Message using a json message
// of type []interface{} or map[string]interface{}
// obj must be previously obtained from a json.Unmarshal
func updateMsg(ctx string, obj interface{}, dm proto.Message, example proto.Message, allowUnknownFields bool) (ce *adapter.ConfigErrors) {
	var enc []byte
	var err error

	if enc, err = json.Marshal(obj); err != nil {
		return ce.Append(ctx, err)
	}
	um := &jsonpb.Unmarshaler{AllowUnknownFields: allowUnknownFields}
	if err = um.Unmarshal(bytes.NewReader(enc), dm); err != nil {
		msg := fmt.Sprintf("%v: %s", err, string(enc))
		if example != nil {
			um := &jsonpb.Marshaler{}
			exampleStr, _ := um.MarshalToString(example) // nolint: gas
			msg += ", example: " + exampleStr
		}
		return ce.Append(ctx, errors.New(msg))
	}

	return nil
}

// processArray and return typed array
func processArray(name string, arr []interface{}, nm proto.Message) (reflect.Value, *adapter.ConfigErrors) {
	var ce *adapter.ConfigErrors
	ptrType := reflect.TypeOf(nm)
	valType := reflect.Indirect(reflect.ValueOf(nm)).Type()
	outarr := reflect.MakeSlice(reflect.SliceOf(ptrType), 0, len(arr))
	for idx, attr := range arr {
		dm := reflect.New(valType).Interface().(proto.Message)

		if cerr := updateMsg(fmt.Sprintf("%s[%d]", name, idx), attr, dm, nm, false); cerr != nil {
			ce = ce.Extend(cerr)
			continue
		}

		outarr = reflect.Append(outarr, reflect.ValueOf(dm))
	}
	return outarr, ce
}

// processMap and return a typed map.
func processMap(name string, arr map[string]interface{}, value proto.Message) (reflect.Value, *adapter.ConfigErrors) {
	var ce *adapter.ConfigErrors
	ptrType := reflect.TypeOf(value)
	valueType := reflect.Indirect(reflect.ValueOf(value)).Type()
	outmap := reflect.MakeMap(reflect.MapOf(reflect.ValueOf("").Type(), ptrType))
	for vname, val := range arr {
		dm := reflect.New(valueType).Interface().(proto.Message)
		if cerr := updateMsg(fmt.Sprintf("%s[%s]", name, vname), val, dm, value, false); cerr != nil {
			ce = ce.Extend(cerr)
			continue
		}
		outmap.SetMapIndex(reflect.ValueOf(vname), reflect.ValueOf(dm))
	}
	return outmap, ce
}

// NewFinder constructs a new Finder for the provided global config.
func NewFinder(cfg *pb.GlobalConfig) Finder {
	f := &finder{
		attributes: make(map[string]*pb.AttributeManifest_AttributeInfo),
	}

	if cfg == nil {
		return f
	}

	for _, manifest := range cfg.Manifests {
		for name, info := range manifest.Attributes {
			f.attributes[name] = info
		}
	}

	return f
}

func (d *finder) GetAttribute(name string) *pb.AttributeManifest_AttributeInfo {
	return d.attributes[name]
}
