package splunk

import (
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/moby/moby/v2/daemon/logger"
	"gotest.tools/v3/assert"
)

// Validate options
func TestValidateLogOpt(t *testing.T) {
	err := ValidateLogOpt(map[string]string{
		splunkURLKey:                  "http://127.0.0.1",
		splunkTokenKey:                "2160C7EF-2CE9-4307-A180-F852B99CF417",
		splunkSourceKey:               "mysource",
		splunkSourceTypeKey:           "mysourcetype",
		splunkIndexKey:                "myindex",
		splunkCAPathKey:               "/usr/cert.pem",
		splunkCANameKey:               "ca_name",
		splunkInsecureSkipVerifyKey:   "true",
		splunkFormatKey:               "json",
		splunkVerifyConnectionKey:     "true",
		splunkGzipCompressionKey:      "true",
		splunkGzipCompressionLevelKey: "1",
		logger.AttrEnv:                "a",
		logger.AttrEnvRegex:           "^foo",
		logger.AttrLabels:             "b",
		logger.AttrLabelsRegex:        "^foo",
		logger.AttrLogTag:             "c",
	})
	if err != nil {
		t.Fatal(err)
	}

	err = ValidateLogOpt(map[string]string{
		"not-supported-option": "a",
	})
	if err == nil {
		t.Fatal("Expecting error on unsupported options")
	}
}

// Driver require user to specify required options
func TestNewMissedConfig(t *testing.T) {
	info := logger.Info{
		Config: map[string]string{},
	}
	_, err := New(info)
	if err == nil {
		t.Fatal("Logger driver should fail when no required parameters specified")
	}
}

// Driver require user to specify splunk-url
func TestNewMissedUrl(t *testing.T) {
	info := logger.Info{
		Config: map[string]string{
			splunkTokenKey: "4642492F-D8BD-47F1-A005-0C08AE4657DF",
		},
	}
	_, err := New(info)
	if err.Error() != "splunk: splunk-url is expected" {
		t.Fatal("Logger driver should fail when no required parameters specified")
	}
}

// Driver require user to specify splunk-token
func TestNewMissedToken(t *testing.T) {
	info := logger.Info{
		Config: map[string]string{
			splunkURLKey: "http://127.0.0.1:8088",
		},
	}
	_, err := New(info)
	if err.Error() != "splunk: splunk-token is expected" {
		t.Fatal("Logger driver should fail when no required parameters specified")
	}
}

func TestNewWithProxy(t *testing.T) {
	proxy := "http://proxy.testing:8888"
	t.Setenv("HTTP_PROXY", proxy)

	// must not be localhost
	splunkURL := "http://example.com:12345"
	logger, err := New(logger.Info{
		Config: map[string]string{
			splunkURLKey:              splunkURL,
			splunkTokenKey:            "token",
			splunkVerifyConnectionKey: "false",
		},
		ContainerID: "containeriid",
	})
	assert.NilError(t, err)
	splunkLogger := logger.(*splunkLoggerInline)

	proxyFunc := splunkLogger.transport.Proxy
	assert.Assert(t, proxyFunc != nil)

	req, err := http.NewRequest(http.MethodGet, splunkURL, http.NoBody)
	assert.NilError(t, err)

	proxyURL, err := proxyFunc(req)
	assert.NilError(t, err)
	assert.Assert(t, proxyURL != nil)
	assert.Equal(t, proxy, proxyURL.String())
}

// Test default settings
func TestDefault(t *testing.T) {
	hec := NewHTTPEventCollectorMock(t)

	go hec.Serve()

	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:   hec.URL(),
			splunkTokenKey: hec.token,
		},
		ContainerID:        "containeriid",
		ContainerName:      "container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
	}

	hostname, err := info.Hostname()
	if err != nil {
		t.Fatal(err)
	}

	loggerDriver, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	if loggerDriver.Name() != driverName {
		t.Fatal("Unexpected logger driver name")
	}

	if !hec.connectionVerified {
		t.Fatal("By default connection should be verified")
	}

	splunkLoggerDriver, ok := loggerDriver.(*splunkLoggerInline)
	if !ok {
		t.Fatal("Unexpected Splunk Logging Driver type")
	}

	if splunkLoggerDriver.url != hec.URL()+"/services/collector/event/1.0" ||
		splunkLoggerDriver.auth != "Splunk "+hec.token ||
		splunkLoggerDriver.nullMessage.Host != hostname ||
		splunkLoggerDriver.nullMessage.Source != "" ||
		splunkLoggerDriver.nullMessage.SourceType != "" ||
		splunkLoggerDriver.nullMessage.Index != "" ||
		splunkLoggerDriver.gzipCompression ||
		splunkLoggerDriver.postMessagesFrequency != defaultPostMessagesFrequency ||
		splunkLoggerDriver.postMessagesBatchSize != defaultPostMessagesBatchSize ||
		splunkLoggerDriver.bufferMaximum != defaultBufferMaximum ||
		cap(splunkLoggerDriver.stream) != defaultStreamChannelSize {
		t.Fatal("Found not default values setup in Splunk Logging Driver.")
	}

	message1Time := time.Now()
	if err := loggerDriver.Log(&logger.Message{Line: []byte("{\"a\":\"b\"}"), Source: "stdout", Timestamp: message1Time}); err != nil {
		t.Fatal(err)
	}
	message2Time := time.Now()
	if err := loggerDriver.Log(&logger.Message{Line: []byte("notajson"), Source: "stdout", Timestamp: message2Time}); err != nil {
		t.Fatal(err)
	}

	err = loggerDriver.Close()
	if err != nil {
		t.Fatal(err)
	}

	if len(hec.messages) != 2 {
		t.Fatal("Expected two messages")
	}

	if *hec.gzipEnabled {
		t.Fatal("Gzip should not be used")
	}

	message1 := hec.messages[0]
	if message1.Time != fmt.Sprintf("%f", float64(message1Time.UnixNano())/float64(time.Second)) ||
		message1.Host != hostname ||
		message1.Source != "" ||
		message1.SourceType != "" ||
		message1.Index != "" {
		t.Fatalf("Unexpected values of message 1 %v", message1)
	}

	if event, err := message1.EventAsMap(); err != nil {
		t.Fatal(err)
	} else {
		if event["line"] != "{\"a\":\"b\"}" ||
			event["source"] != "stdout" ||
			event["tag"] != "containeriid" ||
			len(event) != 3 {
			t.Fatalf("Unexpected event in message %v", event)
		}
	}

	message2 := hec.messages[1]
	if message2.Time != fmt.Sprintf("%f", float64(message2Time.UnixNano())/float64(time.Second)) ||
		message2.Host != hostname ||
		message2.Source != "" ||
		message2.SourceType != "" ||
		message2.Index != "" {
		t.Fatalf("Unexpected values of message 1 %v", message2)
	}

	if event, err := message2.EventAsMap(); err != nil {
		t.Fatal(err)
	} else {
		if event["line"] != "notajson" ||
			event["source"] != "stdout" ||
			event["tag"] != "containeriid" ||
			len(event) != 3 {
			t.Fatalf("Unexpected event in message %v", event)
		}
	}

	err = hec.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Verify inline format with a not default settings for most of options
func TestInlineFormatWithNonDefaultOptions(t *testing.T) {
	hec := NewHTTPEventCollectorMock(t)

	go hec.Serve()

	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:             hec.URL(),
			splunkTokenKey:           hec.token,
			splunkSourceKey:          "mysource",
			splunkSourceTypeKey:      "mysourcetype",
			splunkIndexKey:           "myindex",
			splunkFormatKey:          splunkFormatInline,
			splunkGzipCompressionKey: "true",

			logger.AttrLogTag:   "{{.ImageName}}/{{.Name}}",
			logger.AttrLabels:   "a",
			logger.AttrEnvRegex: "^foo",
		},
		ContainerID:        "containeriid",
		ContainerName:      "/container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
		ContainerLabels: map[string]string{
			"a": "b",
		},
		ContainerEnv: []string{"foo_finder=bar"},
	}

	hostname, err := info.Hostname()
	if err != nil {
		t.Fatal(err)
	}

	loggerDriver, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	if !hec.connectionVerified {
		t.Fatal("By default connection should be verified")
	}

	splunkLoggerDriver, ok := loggerDriver.(*splunkLoggerInline)
	if !ok {
		t.Fatal("Unexpected Splunk Logging Driver type")
	}

	if splunkLoggerDriver.url != hec.URL()+"/services/collector/event/1.0" ||
		splunkLoggerDriver.auth != "Splunk "+hec.token ||
		splunkLoggerDriver.nullMessage.Host != hostname ||
		splunkLoggerDriver.nullMessage.Source != "mysource" ||
		splunkLoggerDriver.nullMessage.SourceType != "mysourcetype" ||
		splunkLoggerDriver.nullMessage.Index != "myindex" ||
		!splunkLoggerDriver.gzipCompression ||
		splunkLoggerDriver.gzipCompressionLevel != gzip.DefaultCompression ||
		splunkLoggerDriver.postMessagesFrequency != defaultPostMessagesFrequency ||
		splunkLoggerDriver.postMessagesBatchSize != defaultPostMessagesBatchSize ||
		splunkLoggerDriver.bufferMaximum != defaultBufferMaximum ||
		cap(splunkLoggerDriver.stream) != defaultStreamChannelSize {
		t.Fatal("Values do not match configuration.")
	}

	messageTime := time.Now()
	if err := loggerDriver.Log(&logger.Message{Line: []byte("1"), Source: "stdout", Timestamp: messageTime}); err != nil {
		t.Fatal(err)
	}

	err = loggerDriver.Close()
	if err != nil {
		t.Fatal(err)
	}

	if len(hec.messages) != 1 {
		t.Fatal("Expected one message")
	}

	if !*hec.gzipEnabled {
		t.Fatal("Gzip should be used")
	}

	message := hec.messages[0]
	if message.Time != fmt.Sprintf("%f", float64(messageTime.UnixNano())/float64(time.Second)) ||
		message.Host != hostname ||
		message.Source != "mysource" ||
		message.SourceType != "mysourcetype" ||
		message.Index != "myindex" {
		t.Fatalf("Unexpected values of message %v", message)
	}

	if event, err := message.EventAsMap(); err != nil {
		t.Fatal(err)
	} else {
		if event["line"] != "1" ||
			event["source"] != "stdout" ||
			event["tag"] != "container_image_name/container_name" ||
			event["attrs"].(map[string]any)["a"] != "b" ||
			event["attrs"].(map[string]any)["foo_finder"] != "bar" ||
			len(event) != 4 {
			t.Fatalf("Unexpected event in message %v", event)
		}
	}

	err = hec.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Verify JSON format
func TestJsonFormat(t *testing.T) {
	hec := NewHTTPEventCollectorMock(t)

	go hec.Serve()

	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:                  hec.URL(),
			splunkTokenKey:                hec.token,
			splunkFormatKey:               splunkFormatJSON,
			splunkGzipCompressionKey:      "true",
			splunkGzipCompressionLevelKey: "1",
		},
		ContainerID:        "containeriid",
		ContainerName:      "/container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
	}

	hostname, err := info.Hostname()
	if err != nil {
		t.Fatal(err)
	}

	loggerDriver, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	if !hec.connectionVerified {
		t.Fatal("By default connection should be verified")
	}

	splunkLoggerDriver, ok := loggerDriver.(*splunkLoggerJSON)
	if !ok {
		t.Fatal("Unexpected Splunk Logging Driver type")
	}

	if splunkLoggerDriver.url != hec.URL()+"/services/collector/event/1.0" ||
		splunkLoggerDriver.auth != "Splunk "+hec.token ||
		splunkLoggerDriver.nullMessage.Host != hostname ||
		splunkLoggerDriver.nullMessage.Source != "" ||
		splunkLoggerDriver.nullMessage.SourceType != "" ||
		splunkLoggerDriver.nullMessage.Index != "" ||
		!splunkLoggerDriver.gzipCompression ||
		splunkLoggerDriver.gzipCompressionLevel != gzip.BestSpeed ||
		splunkLoggerDriver.postMessagesFrequency != defaultPostMessagesFrequency ||
		splunkLoggerDriver.postMessagesBatchSize != defaultPostMessagesBatchSize ||
		splunkLoggerDriver.bufferMaximum != defaultBufferMaximum ||
		cap(splunkLoggerDriver.stream) != defaultStreamChannelSize {
		t.Fatal("Values do not match configuration.")
	}

	message1Time := time.Now()
	if err := loggerDriver.Log(&logger.Message{Line: []byte("{\"a\":\"b\"}"), Source: "stdout", Timestamp: message1Time}); err != nil {
		t.Fatal(err)
	}
	message2Time := time.Now()
	if err := loggerDriver.Log(&logger.Message{Line: []byte("notjson"), Source: "stdout", Timestamp: message2Time}); err != nil {
		t.Fatal(err)
	}

	err = loggerDriver.Close()
	if err != nil {
		t.Fatal(err)
	}

	if len(hec.messages) != 2 {
		t.Fatal("Expected two messages")
	}

	message1 := hec.messages[0]
	if message1.Time != fmt.Sprintf("%f", float64(message1Time.UnixNano())/float64(time.Second)) ||
		message1.Host != hostname ||
		message1.Source != "" ||
		message1.SourceType != "" ||
		message1.Index != "" {
		t.Fatalf("Unexpected values of message 1 %v", message1)
	}

	if event, err := message1.EventAsMap(); err != nil {
		t.Fatal(err)
	} else {
		if event["line"].(map[string]any)["a"] != "b" ||
			event["source"] != "stdout" ||
			event["tag"] != "containeriid" ||
			len(event) != 3 {
			t.Fatalf("Unexpected event in message 1 %v", event)
		}
	}

	message2 := hec.messages[1]
	if message2.Time != fmt.Sprintf("%f", float64(message2Time.UnixNano())/float64(time.Second)) ||
		message2.Host != hostname ||
		message2.Source != "" ||
		message2.SourceType != "" ||
		message2.Index != "" {
		t.Fatalf("Unexpected values of message 2 %v", message2)
	}

	// If message cannot be parsed as JSON - it should be sent as a line
	if event, err := message2.EventAsMap(); err != nil {
		t.Fatal(err)
	} else {
		if event["line"] != "notjson" ||
			event["source"] != "stdout" ||
			event["tag"] != "containeriid" ||
			len(event) != 3 {
			t.Fatalf("Unexpected event in message 2 %v", event)
		}
	}

	err = hec.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Verify raw format
func TestRawFormat(t *testing.T) {
	hec := NewHTTPEventCollectorMock(t)

	go hec.Serve()

	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:    hec.URL(),
			splunkTokenKey:  hec.token,
			splunkFormatKey: splunkFormatRaw,
		},
		ContainerID:        "containeriid",
		ContainerName:      "/container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
	}

	hostname, err := info.Hostname()
	assert.NilError(t, err)

	loggerDriver, err := New(info)
	assert.NilError(t, err)

	if !hec.connectionVerified {
		t.Fatal("By default connection should be verified")
	}

	splunkLoggerDriver, ok := loggerDriver.(*splunkLoggerRaw)
	if !ok {
		t.Fatal("Unexpected Splunk Logging Driver type")
	}

	if splunkLoggerDriver.url != hec.URL()+"/services/collector/event/1.0" ||
		splunkLoggerDriver.auth != "Splunk "+hec.token ||
		splunkLoggerDriver.nullMessage.Host != hostname ||
		splunkLoggerDriver.nullMessage.Source != "" ||
		splunkLoggerDriver.nullMessage.SourceType != "" ||
		splunkLoggerDriver.nullMessage.Index != "" ||
		splunkLoggerDriver.gzipCompression ||
		splunkLoggerDriver.postMessagesFrequency != defaultPostMessagesFrequency ||
		splunkLoggerDriver.postMessagesBatchSize != defaultPostMessagesBatchSize ||
		splunkLoggerDriver.bufferMaximum != defaultBufferMaximum ||
		cap(splunkLoggerDriver.stream) != defaultStreamChannelSize ||
		string(splunkLoggerDriver.prefix) != "containeriid " {
		t.Fatal("Values do not match configuration.")
	}

	message1Time := time.Now()
	if err := loggerDriver.Log(&logger.Message{Line: []byte("{\"a\":\"b\"}"), Source: "stdout", Timestamp: message1Time}); err != nil {
		t.Fatal(err)
	}
	message2Time := time.Now()
	if err := loggerDriver.Log(&logger.Message{Line: []byte("notjson"), Source: "stdout", Timestamp: message2Time}); err != nil {
		t.Fatal(err)
	}

	err = loggerDriver.Close()
	if err != nil {
		t.Fatal(err)
	}

	if len(hec.messages) != 2 {
		t.Fatal("Expected two messages")
	}

	message1 := hec.messages[0]
	if message1.Time != fmt.Sprintf("%f", float64(message1Time.UnixNano())/float64(time.Second)) ||
		message1.Host != hostname ||
		message1.Source != "" ||
		message1.SourceType != "" ||
		message1.Index != "" {
		t.Fatalf("Unexpected values of message 1 %v", message1)
	}

	if event, err := message1.EventAsString(); err != nil {
		t.Fatal(err)
	} else {
		if event != "containeriid {\"a\":\"b\"}" {
			t.Fatalf("Unexpected event in message 1 %v", event)
		}
	}

	message2 := hec.messages[1]
	if message2.Time != fmt.Sprintf("%f", float64(message2Time.UnixNano())/float64(time.Second)) ||
		message2.Host != hostname ||
		message2.Source != "" ||
		message2.SourceType != "" ||
		message2.Index != "" {
		t.Fatalf("Unexpected values of message 2 %v", message2)
	}

	if event, err := message2.EventAsString(); err != nil {
		t.Fatal(err)
	} else {
		if event != "containeriid notjson" {
			t.Fatalf("Unexpected event in message 1 %v", event)
		}
	}

	err = hec.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Verify raw format with labels
func TestRawFormatWithLabels(t *testing.T) {
	hec := NewHTTPEventCollectorMock(t)

	go hec.Serve()

	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:    hec.URL(),
			splunkTokenKey:  hec.token,
			splunkFormatKey: splunkFormatRaw,

			logger.AttrLabels: "a",
		},
		ContainerID:        "containeriid",
		ContainerName:      "/container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
		ContainerLabels: map[string]string{
			"a": "b",
		},
	}

	hostname, err := info.Hostname()
	if err != nil {
		t.Fatal(err)
	}

	loggerDriver, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	if !hec.connectionVerified {
		t.Fatal("By default connection should be verified")
	}

	splunkLoggerDriver, ok := loggerDriver.(*splunkLoggerRaw)
	if !ok {
		t.Fatal("Unexpected Splunk Logging Driver type")
	}

	if splunkLoggerDriver.url != hec.URL()+"/services/collector/event/1.0" ||
		splunkLoggerDriver.auth != "Splunk "+hec.token ||
		splunkLoggerDriver.nullMessage.Host != hostname ||
		splunkLoggerDriver.nullMessage.Source != "" ||
		splunkLoggerDriver.nullMessage.SourceType != "" ||
		splunkLoggerDriver.nullMessage.Index != "" ||
		splunkLoggerDriver.gzipCompression ||
		splunkLoggerDriver.postMessagesFrequency != defaultPostMessagesFrequency ||
		splunkLoggerDriver.postMessagesBatchSize != defaultPostMessagesBatchSize ||
		splunkLoggerDriver.bufferMaximum != defaultBufferMaximum ||
		cap(splunkLoggerDriver.stream) != defaultStreamChannelSize ||
		string(splunkLoggerDriver.prefix) != "containeriid a=b " {
		t.Fatal("Values do not match configuration.")
	}

	message1Time := time.Now()
	if err := loggerDriver.Log(&logger.Message{Line: []byte("{\"a\":\"b\"}"), Source: "stdout", Timestamp: message1Time}); err != nil {
		t.Fatal(err)
	}
	message2Time := time.Now()
	if err := loggerDriver.Log(&logger.Message{Line: []byte("notjson"), Source: "stdout", Timestamp: message2Time}); err != nil {
		t.Fatal(err)
	}

	err = loggerDriver.Close()
	if err != nil {
		t.Fatal(err)
	}

	if len(hec.messages) != 2 {
		t.Fatal("Expected two messages")
	}

	message1 := hec.messages[0]
	if message1.Time != fmt.Sprintf("%f", float64(message1Time.UnixNano())/float64(time.Second)) ||
		message1.Host != hostname ||
		message1.Source != "" ||
		message1.SourceType != "" ||
		message1.Index != "" {
		t.Fatalf("Unexpected values of message 1 %v", message1)
	}

	if event, err := message1.EventAsString(); err != nil {
		t.Fatal(err)
	} else {
		if event != "containeriid a=b {\"a\":\"b\"}" {
			t.Fatalf("Unexpected event in message 1 %v", event)
		}
	}

	message2 := hec.messages[1]
	if message2.Time != fmt.Sprintf("%f", float64(message2Time.UnixNano())/float64(time.Second)) ||
		message2.Host != hostname ||
		message2.Source != "" ||
		message2.SourceType != "" ||
		message2.Index != "" {
		t.Fatalf("Unexpected values of message 2 %v", message2)
	}

	if event, err := message2.EventAsString(); err != nil {
		t.Fatal(err)
	} else {
		if event != "containeriid a=b notjson" {
			t.Fatalf("Unexpected event in message 2 %v", event)
		}
	}

	err = hec.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Verify that Splunk Logging Driver can accept tag="" which will allow to send raw messages
// in the same way we get them in stdout/stderr
func TestRawFormatWithoutTag(t *testing.T) {
	hec := NewHTTPEventCollectorMock(t)

	go hec.Serve()

	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:    hec.URL(),
			splunkTokenKey:  hec.token,
			splunkFormatKey: splunkFormatRaw,

			logger.AttrLogTag: "",
		},
		ContainerID:        "containeriid",
		ContainerName:      "/container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
	}

	hostname, err := info.Hostname()
	if err != nil {
		t.Fatal(err)
	}

	loggerDriver, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	if !hec.connectionVerified {
		t.Fatal("By default connection should be verified")
	}

	splunkLoggerDriver, ok := loggerDriver.(*splunkLoggerRaw)
	if !ok {
		t.Fatal("Unexpected Splunk Logging Driver type")
	}

	if splunkLoggerDriver.url != hec.URL()+"/services/collector/event/1.0" ||
		splunkLoggerDriver.auth != "Splunk "+hec.token ||
		splunkLoggerDriver.nullMessage.Host != hostname ||
		splunkLoggerDriver.nullMessage.Source != "" ||
		splunkLoggerDriver.nullMessage.SourceType != "" ||
		splunkLoggerDriver.nullMessage.Index != "" ||
		splunkLoggerDriver.gzipCompression ||
		splunkLoggerDriver.postMessagesFrequency != defaultPostMessagesFrequency ||
		splunkLoggerDriver.postMessagesBatchSize != defaultPostMessagesBatchSize ||
		splunkLoggerDriver.bufferMaximum != defaultBufferMaximum ||
		cap(splunkLoggerDriver.stream) != defaultStreamChannelSize ||
		string(splunkLoggerDriver.prefix) != "" {
		t.Log(string(splunkLoggerDriver.prefix) + "a")
		t.Fatal("Values do not match configuration.")
	}

	message1Time := time.Now()
	if err := loggerDriver.Log(&logger.Message{Line: []byte("{\"a\":\"b\"}"), Source: "stdout", Timestamp: message1Time}); err != nil {
		t.Fatal(err)
	}
	message2Time := time.Now()
	if err := loggerDriver.Log(&logger.Message{Line: []byte("notjson"), Source: "stdout", Timestamp: message2Time}); err != nil {
		t.Fatal(err)
	}
	message3Time := time.Now()
	if err := loggerDriver.Log(&logger.Message{Line: []byte(" "), Source: "stdout", Timestamp: message3Time}); err != nil {
		t.Fatal(err)
	}

	err = loggerDriver.Close()
	if err != nil {
		t.Fatal(err)
	}

	// message3 would have an empty or whitespace only string in the "event" field
	// both of which are not acceptable to HEC
	// thus here we must expect 2 messages, not 3
	if len(hec.messages) != 2 {
		t.Fatal("Expected two messages")
	}

	message1 := hec.messages[0]
	if message1.Time != fmt.Sprintf("%f", float64(message1Time.UnixNano())/float64(time.Second)) ||
		message1.Host != hostname ||
		message1.Source != "" ||
		message1.SourceType != "" ||
		message1.Index != "" {
		t.Fatalf("Unexpected values of message 1 %v", message1)
	}

	if event, err := message1.EventAsString(); err != nil {
		t.Fatal(err)
	} else {
		if event != "{\"a\":\"b\"}" {
			t.Fatalf("Unexpected event in message 1 %v", event)
		}
	}

	message2 := hec.messages[1]
	if message2.Time != fmt.Sprintf("%f", float64(message2Time.UnixNano())/float64(time.Second)) ||
		message2.Host != hostname ||
		message2.Source != "" ||
		message2.SourceType != "" ||
		message2.Index != "" {
		t.Fatalf("Unexpected values of message 2 %v", message2)
	}

	if event, err := message2.EventAsString(); err != nil {
		t.Fatal(err)
	} else {
		if event != "notjson" {
			t.Fatalf("Unexpected event in message 2 %v", event)
		}
	}

	err = hec.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Verify that we will send messages in batches with default batching parameters,
// but change frequency to be sure that numOfRequests will match expected 17 requests
func TestBatching(t *testing.T) {
	t.Setenv(envVarPostMessagesFrequency, "10h")

	hec := NewHTTPEventCollectorMock(t)

	go hec.Serve()

	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:   hec.URL(),
			splunkTokenKey: hec.token,
		},
		ContainerID:        "containeriid",
		ContainerName:      "/container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
	}

	loggerDriver, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	for i := range defaultStreamChannelSize * 4 {
		if err := loggerDriver.Log(&logger.Message{Line: []byte(strconv.Itoa(i)), Source: "stdout", Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	err = loggerDriver.Close()
	if err != nil {
		t.Fatal(err)
	}

	if len(hec.messages) != defaultStreamChannelSize*4 {
		t.Fatal("Not all messages delivered")
	}

	for i, message := range hec.messages {
		if event, err := message.EventAsMap(); err != nil {
			t.Fatal(err)
		} else {
			if event["line"] != strconv.Itoa(i) {
				t.Fatalf("Unexpected event in message %v", event)
			}
		}
	}

	// 1 to verify connection and 16 batches
	if hec.numOfRequests != 17 {
		t.Fatalf("Unexpected number of requests %d", hec.numOfRequests)
	}

	err = hec.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Verify that test is using time to fire events not rare than specified frequency
func TestFrequency(t *testing.T) {
	t.Setenv(envVarPostMessagesFrequency, "5ms")

	hec := NewHTTPEventCollectorMock(t)

	go hec.Serve()

	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:   hec.URL(),
			splunkTokenKey: hec.token,
		},
		ContainerID:        "containeriid",
		ContainerName:      "/container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
	}

	loggerDriver, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	for i := range 10 {
		if err := loggerDriver.Log(&logger.Message{Line: []byte(strconv.Itoa(i)), Source: "stdout", Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(15 * time.Millisecond)
	}

	err = loggerDriver.Close()
	if err != nil {
		t.Fatal(err)
	}

	if len(hec.messages) != 10 {
		t.Fatal("Not all messages delivered")
	}

	for i, message := range hec.messages {
		if event, err := message.EventAsMap(); err != nil {
			t.Fatal(err)
		} else {
			if event["line"] != strconv.Itoa(i) {
				t.Fatalf("Unexpected event in message %v", event)
			}
		}
	}

	// 1 to verify connection and 10 to verify that we have sent messages with required frequency,
	// but because frequency is too small (to keep test quick), instead of 11, use 9 if context switches will be slow
	expectedRequests := 9
	if runtime.GOOS == "windows" {
		// sometimes in Windows, this test fails with number of requests showing 8. So be more conservative.
		expectedRequests = 7
	}
	if hec.numOfRequests < expectedRequests {
		t.Fatalf("Unexpected number of requests %d", hec.numOfRequests)
	}

	err = hec.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Simulate behavior similar to first version of Splunk Logging Driver, when we were sending one message
// per request
func TestOneMessagePerRequest(t *testing.T) {
	t.Setenv(envVarPostMessagesFrequency, "10h")
	t.Setenv(envVarPostMessagesBatchSize, "1")
	t.Setenv(envVarBufferMaximum, "1")
	t.Setenv(envVarStreamChannelSize, "0")

	hec := NewHTTPEventCollectorMock(t)

	go hec.Serve()

	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:   hec.URL(),
			splunkTokenKey: hec.token,
		},
		ContainerID:        "containeriid",
		ContainerName:      "/container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
	}

	loggerDriver, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	for i := range 10 {
		if err := loggerDriver.Log(&logger.Message{Line: []byte(strconv.Itoa(i)), Source: "stdout", Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	err = loggerDriver.Close()
	if err != nil {
		t.Fatal(err)
	}

	if len(hec.messages) != 10 {
		t.Fatal("Not all messages delivered")
	}

	for i, message := range hec.messages {
		if event, err := message.EventAsMap(); err != nil {
			t.Fatal(err)
		} else {
			if event["line"] != strconv.Itoa(i) {
				t.Fatalf("Unexpected event in message %v", event)
			}
		}
	}

	// 1 to verify connection and 10 messages
	if hec.numOfRequests != 11 {
		t.Fatalf("Unexpected number of requests %d", hec.numOfRequests)
	}

	err = hec.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Driver should not be created when HEC is unresponsive
func TestVerify(t *testing.T) {
	hec := NewHTTPEventCollectorMock(t)
	hec.simulateServerError = true
	go hec.Serve()

	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:   hec.URL(),
			splunkTokenKey: hec.token,
		},
		ContainerID:        "containeriid",
		ContainerName:      "/container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
	}

	_, err := New(info)
	if err == nil {
		t.Fatal("Expecting driver to fail, when server is unresponsive")
	}

	err = hec.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Verify that user can specify to skip verification that Splunk HEC is working.
// Also in this test we verify retry logic.
func TestSkipVerify(t *testing.T) {
	hec := NewHTTPEventCollectorMock(t)
	hec.simulateServerError = true
	go hec.Serve()

	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:              hec.URL(),
			splunkTokenKey:            hec.token,
			splunkVerifyConnectionKey: "false",
		},
		ContainerID:        "containeriid",
		ContainerName:      "/container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
	}

	loggerDriver, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	if hec.connectionVerified {
		t.Fatal("Connection should not be verified")
	}

	for i := range defaultStreamChannelSize * 2 {
		if err := loggerDriver.Log(&logger.Message{Line: []byte(strconv.Itoa(i)), Source: "stdout", Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	if len(hec.messages) != 0 {
		t.Fatal("No messages should be accepted at this point")
	}

	hec.simulateErr(false)

	for i := defaultStreamChannelSize * 2; i < defaultStreamChannelSize*4; i++ {
		if err := loggerDriver.Log(&logger.Message{Line: []byte(strconv.Itoa(i)), Source: "stdout", Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	err = loggerDriver.Close()
	if err != nil {
		t.Fatal(err)
	}

	if len(hec.messages) != defaultStreamChannelSize*4 {
		t.Fatal("Not all messages delivered")
	}

	for i, message := range hec.messages {
		if event, err := message.EventAsMap(); err != nil {
			t.Fatal(err)
		} else {
			if event["line"] != strconv.Itoa(i) {
				t.Fatalf("Unexpected event in message %v", event)
			}
		}
	}

	err = hec.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Verify logic for when we filled whole buffer
func TestBufferMaximum(t *testing.T) {
	t.Setenv(envVarPostMessagesBatchSize, "2")
	t.Setenv(envVarBufferMaximum, "10")
	t.Setenv(envVarStreamChannelSize, "0")

	hec := NewHTTPEventCollectorMock(t)
	hec.simulateErr(true)
	go hec.Serve()

	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:              hec.URL(),
			splunkTokenKey:            hec.token,
			splunkVerifyConnectionKey: "false",
		},
		ContainerID:        "containeriid",
		ContainerName:      "/container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
	}

	loggerDriver, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	if hec.connectionVerified {
		t.Fatal("Connection should not be verified")
	}

	for i := range 11 {
		if err := loggerDriver.Log(&logger.Message{Line: []byte(strconv.Itoa(i)), Source: "stdout", Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	if len(hec.messages) != 0 {
		t.Fatal("No messages should be accepted at this point")
	}

	hec.simulateServerError = false

	err = loggerDriver.Close()
	if err != nil {
		t.Fatal(err)
	}

	if len(hec.messages) != 9 {
		t.Fatalf("Expected # of messages %d, got %d", 9, len(hec.messages))
	}

	// First 1000 messages are written to daemon log when buffer was full
	for i, message := range hec.messages {
		if event, err := message.EventAsMap(); err != nil {
			t.Fatal(err)
		} else {
			if event["line"] != strconv.Itoa(i+2) {
				t.Fatalf("Unexpected event in message %v", event)
			}
		}
	}

	err = hec.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Verify that it returns an error when the channel is full, so that the
// RingLogger can log the error and increment the error metric
func TestBufferMaximumNonBlocking(t *testing.T) {
	t.Setenv(envVarStreamChannelSize, "1")

	hec := NewHTTPEventCollectorMock(t)
	hec.simulateErr(true)
	go hec.Serve()

	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:              hec.URL(),
			splunkTokenKey:            hec.token,
			splunkVerifyConnectionKey: "false",
			mode:                      "non-blocking",
		},
		ContainerID:        "containeriid",
		ContainerName:      "/container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
	}

	loggerDriver, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	if hec.connectionVerified {
		t.Fatal("Connection should not be verified")
	}

	if err := loggerDriver.Log(&logger.Message{Line: []byte("1"), Source: "stdout", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}

	if err := loggerDriver.Log(&logger.Message{Line: []byte("2"), Source: "stdout", Timestamp: time.Now()}); err == nil {
		t.Fatal("Expected error when buffer is full")
	}

	err = hec.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Verify that we are not blocking close when HEC is down for the whole time
func TestServerAlwaysDown(t *testing.T) {
	t.Setenv(envVarPostMessagesBatchSize, "2")
	t.Setenv(envVarBufferMaximum, "4")
	t.Setenv(envVarStreamChannelSize, "0")

	hec := NewHTTPEventCollectorMock(t)
	hec.simulateServerError = true
	go hec.Serve()

	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:              hec.URL(),
			splunkTokenKey:            hec.token,
			splunkVerifyConnectionKey: "false",
		},
		ContainerID:        "containeriid",
		ContainerName:      "/container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
	}

	loggerDriver, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	if hec.connectionVerified {
		t.Fatal("Connection should not be verified")
	}

	for i := range 5 {
		if err := loggerDriver.Log(&logger.Message{Line: []byte(strconv.Itoa(i)), Source: "stdout", Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	err = loggerDriver.Close()
	if err != nil {
		t.Fatal(err)
	}

	if len(hec.messages) != 0 {
		t.Fatal("No messages should be sent")
	}

	err = hec.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Cannot send messages after we close driver
func TestCannotSendAfterClose(t *testing.T) {
	hec := NewHTTPEventCollectorMock(t)
	go hec.Serve()

	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:   hec.URL(),
			splunkTokenKey: hec.token,
		},
		ContainerID:        "containeriid",
		ContainerName:      "/container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
	}

	loggerDriver, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	if err := loggerDriver.Log(&logger.Message{Line: []byte("message1"), Source: "stdout", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}

	err = loggerDriver.Close()
	if err != nil {
		t.Fatal(err)
	}

	if err := loggerDriver.Log(&logger.Message{Line: []byte("message2"), Source: "stdout", Timestamp: time.Now()}); err == nil {
		t.Fatal("Driver should not allow to send messages after close")
	}

	if len(hec.messages) != 1 {
		t.Fatal("Only one message should be sent")
	}

	message := hec.messages[0]
	if event, err := message.EventAsMap(); err != nil {
		t.Fatal(err)
	} else {
		if event["line"] != "message1" {
			t.Fatalf("Unexpected event in message %v", event)
		}
	}

	err = hec.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeadlockOnBlockedEndpoint(t *testing.T) {
	hec := NewHTTPEventCollectorMock(t)
	go hec.Serve()
	info := logger.Info{
		Config: map[string]string{
			splunkURLKey:   hec.URL(),
			splunkTokenKey: hec.token,
		},
		ContainerID:        "containeriid",
		ContainerName:      "/container_name",
		ContainerImageID:   "contaimageid",
		ContainerImageName: "container_image_name",
	}

	l, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	ctx, unblock := context.WithCancel(t.Context())
	hec.withBlock(ctx)
	defer unblock()

	batchSendTimeout = 1 * time.Second

	if err := l.Log(&logger.Message{}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		l.Close()
		close(done)
	}()

	select {
	case <-time.After(60 * time.Second):
		buf := make([]byte, 1e6)
		buf = buf[:runtime.Stack(buf, true)]
		t.Logf("STACK DUMP: \n\n%s\n\n", string(buf))
		t.Fatal("timeout waiting for close to finish")
	case <-done:
	}
}
