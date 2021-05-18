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

package plugin

import (
	"strings"

	"github.com/gogo/protobuf/protoc-gen-gogo/descriptor"
	"github.com/gogo/protobuf/protoc-gen-gogo/generator"
)

type ttrpcGenerator struct {
	*generator.Generator
	generator.PluginImports

	typeurlPkg generator.Single
	ttrpcPkg   generator.Single
	contextPkg generator.Single
}

func init() {
	generator.RegisterPlugin(new(ttrpcGenerator))
}

func (p *ttrpcGenerator) Name() string {
	return "ttrpc"
}

func (p *ttrpcGenerator) Init(g *generator.Generator) {
	p.Generator = g
}

func (p *ttrpcGenerator) Generate(file *generator.FileDescriptor) {
	p.PluginImports = generator.NewPluginImports(p.Generator)
	p.contextPkg = p.NewImport("context")
	p.typeurlPkg = p.NewImport("github.com/containerd/typeurl")
	p.ttrpcPkg = p.NewImport("github.com/containerd/ttrpc")

	for _, service := range file.GetService() {
		serviceName := service.GetName()
		if pkg := file.GetPackage(); pkg != "" {
			serviceName = pkg + "." + serviceName
		}

		p.genService(serviceName, service)
	}
}

func (p *ttrpcGenerator) genService(fullName string, service *descriptor.ServiceDescriptorProto) {
	serviceName := service.GetName() + "Service"
	p.P()
	p.P("type ", serviceName, " interface{")
	p.In()
	for _, method := range service.Method {
		p.P(method.GetName(),
			"(ctx ", p.contextPkg.Use(), ".Context, ",
			"req *", p.typeName(method.GetInputType()), ") ",
			"(*", p.typeName(method.GetOutputType()), ", error)")

	}
	p.Out()
	p.P("}")

	p.P()
	// registration method
	p.P("func Register", serviceName, "(srv *", p.ttrpcPkg.Use(), ".Server, svc ", serviceName, ") {")
	p.In()
	p.P(`srv.Register("`, fullName, `", map[string]`, p.ttrpcPkg.Use(), ".Method{")
	p.In()
	for _, method := range service.Method {
		p.P(`"`, method.GetName(), `": `, `func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {`)
		p.In()
		p.P("var req ", p.typeName(method.GetInputType()))
		p.P(`if err := unmarshal(&req); err != nil {`)
		p.In()
		p.P(`return nil, err`)
		p.Out()
		p.P(`}`)
		p.P("return svc.", method.GetName(), "(ctx, &req)")
		p.Out()
		p.P("},")
	}
	p.Out()
	p.P("})")
	p.Out()
	p.P("}")

	clientType := service.GetName() + "Client"
	clientStructType := strings.ToLower(clientType[:1]) + clientType[1:]
	p.P()
	p.P("type ", clientStructType, " struct{")
	p.In()
	p.P("client *", p.ttrpcPkg.Use(), ".Client")
	p.Out()
	p.P("}")
	p.P()
	p.P("func New", clientType, "(client *", p.ttrpcPkg.Use(), ".Client)", serviceName, "{")
	p.In()
	p.P("return &", clientStructType, "{")
	p.In()
	p.P("client: client,")
	p.Out()
	p.P("}")
	p.Out()
	p.P("}")
	p.P()
	for _, method := range service.Method {
		p.P()
		p.P("func (c *", clientStructType, ") ", method.GetName(),
			"(ctx ", p.contextPkg.Use(), ".Context, ",
			"req *", p.typeName(method.GetInputType()), ") ",
			"(*", p.typeName(method.GetOutputType()), ", error) {")
		p.In()
		p.P("var resp ", p.typeName(method.GetOutputType()))
		p.P("if err := c.client.Call(ctx, ", `"`+fullName+`", `, `"`+method.GetName()+`"`, ", req, &resp); err != nil {")
		p.In()
		p.P("return nil, err")
		p.Out()
		p.P("}")
		p.P("return &resp, nil")
		p.Out()
		p.P("}")
	}
}

func (p *ttrpcGenerator) objectNamed(name string) generator.Object {
	p.Generator.RecordTypeUse(name)
	return p.Generator.ObjectNamed(name)
}

func (p *ttrpcGenerator) typeName(str string) string {
	return p.Generator.TypeName(p.objectNamed(str))
}
