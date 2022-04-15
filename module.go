// MIT License
//
// Copyright (c) 2021 Iván Szkiba
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package cache

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"go.k6.io/k6/js/common"
	"go.k6.io/k6/js/modules"
	"go.k6.io/k6/metrics"
	"go.k6.io/k6/output"
)

const (
	moduleName   = "cache"
	metricPrefix = "xk6_" + moduleName
)

var envKey = "XK6_" + strings.ToUpper(moduleName)

// Register the extensions on module initialization.
func init() {
	file := os.Getenv(envKey)
	mod := New(file, http.DefaultTransport, logrus.StandardLogger())

	modules.Register("k6/x/"+moduleName, mod)
	output.RegisterExtension(moduleName, func(params output.Params) (output.Output, error) {
		mod.log = params.Logger

		return mod, nil
	})

	if file != "" {
		http.DefaultTransport = mod
	}
}

// RootModule is the global module object type. It is instantiated once per test
// run and will be used to create k6/x/cache module instances for each VU.
type RootModule struct {
	log       logrus.FieldLogger
	cache     *Cache
	recorder  *recorder
	transport http.RoundTripper
	hit       uint32
	miss      uint32
	entries   uint32
}

// ModuleInstance represents an instance of the module for every VU.
type ModuleInstance struct {
	vu             modules.VU
	metricRegistry *metrics.Registry
	rootModule     *RootModule
	exports        map[string]interface{}
}

// Ensure the interfaces are implemented correctly.
var (
	_ modules.Instance = &ModuleInstance{}
	_ modules.Module   = &RootModule{}
)

// NewModuleInstance implements the modules.Module interface and returns
// a new instance for each VU.
func (r *RootModule) NewModuleInstance(vu modules.VU) modules.Instance {
	mi := &ModuleInstance{
		vu:             vu,
		metricRegistry: vu.InitEnv().Registry,
		rootModule:     r,
		exports:        make(map[string]interface{}),
	}

	return mi
}

// Exports implements the modules.Instance interface and returns the exports
// of the JS module.
func (mi *ModuleInstance) Exports() modules.Exports {
	return modules.Exports{
		Default: mi,
	}
}

func New(file string, transport http.RoundTripper, log logrus.FieldLogger) *RootModule {
	mod := new(RootModule)

	mod.log = log

	if file == "" {
		return mod
	}

	mod.transport = transport

	if _, err := os.Stat(file); err == nil {
		if mod.cache, err = newCache(file); err != nil {
			log.Error(err)
		} else {
			mod.entries = uint32(mod.cache.EntriesLength())
		}
	} else {
		mod.recorder = newRecorder(file)
	}

	return mod
}

func (m *RootModule) Description() string {
	if m.recorder == nil {
		return "cache (-)"
	}

	return fmt.Sprintf("cache (%s)", m.recorder.file)
}

func (m *RootModule) Start() (err error) {
	if m.recorder != nil {
		return m.recorder.save()
	}

	return nil
}

func (m *RootModule) Stop() error {
	return nil
}

func (m *RootModule) AddMetricSamples(_ []metrics.SampleContainer) {
}

func (mi *ModuleInstance) Measure(prefix string) bool {
	state := mi.vu.State()

	if prefix == "" {
		prefix = metricPrefix
	}

	return metrics.PushIfNotDone(mi.vu.Context(), state.Samples, metrics.Samples{
		newSample(mi.getMetric(prefix, "hit"), mi.rootModule.hit),
		newSample(mi.getMetric(prefix, "miss"), mi.rootModule.miss),
		newSample(mi.getMetric(prefix, "entry"), mi.rootModule.entries),
	})
}

func newSample(m *metrics.Metric, value uint32) metrics.Sample {
	return metrics.Sample{
		Metric: m,
		Tags:   nil,
		Time:   time.Now(),
		Value:  float64(value),
	}
}

func (mi *ModuleInstance) getMetric(prefix, name string) *metrics.Metric {
	key := fmt.Sprintf("%s_%s_count", prefix, name)

	if m := mi.metricRegistry.Get(key); m != nil {
		return m
	}

	m, err := mi.metricRegistry.NewMetric(key, metrics.Counter)
	if err != nil {
		common.Throw(mi.vu.Runtime(), err)
	}

	return m
}

func (m *RootModule) RoundTrip(req *http.Request) (*http.Response, error) {
	if m.cache != nil {
		if resp := m.cache.get(req); resp != nil {
			atomic.AddUint32(&m.hit, 1)

			return resp, nil
		}

		atomic.AddUint32(&m.miss, 1)
	}

	resp, err := m.transport.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	if resp.StatusCode == http.StatusOK && m.recorder != nil {
		return m.recorder.put(req, resp), nil
	}

	return resp, err
}

func newResponse(req *http.Request, status int, body []byte) *http.Response {
	return &http.Response{
		Status:        http.StatusText(status),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Body:          ioutil.NopCloser(bytes.NewBuffer(body)),
		ContentLength: int64(len(body)),
		Request:       req,
		Header:        make(http.Header),
	}
}
