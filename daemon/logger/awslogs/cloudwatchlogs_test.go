package awslogs // import "github.com/docker/docker/daemon/logger/awslogs"

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/loggerutils"
	"github.com/docker/docker/dockerversion"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

const (
	groupName         = "groupName"
	streamName        = "streamName"
	sequenceToken     = "sequenceToken"
	nextSequenceToken = "nextSequenceToken"
	logline           = "this is a log line\r"
	multilineLogline  = "2017-01-01 01:01:44 This is a multiline log entry\r"
)

// Generates i multi-line events each with j lines
func (l *logStream) logGenerator(lineCount int, multilineCount int) {
	for i := 0; i < multilineCount; i++ {
		l.Log(&logger.Message{
			Line:      []byte(multilineLogline),
			Timestamp: time.Time{},
		})
		for j := 0; j < lineCount; j++ {
			l.Log(&logger.Message{
				Line:      []byte(logline),
				Timestamp: time.Time{},
			})
		}
	}
}

func testEventBatch(events []wrappedEvent) *eventBatch {
	batch := newEventBatch()
	for _, event := range events {
		eventlen := len([]byte(*event.inputLogEvent.Message))
		batch.add(event, eventlen)
	}
	return batch
}

func TestNewStreamConfig(t *testing.T) {
	tests := []struct {
		logStreamName      string
		logGroupName       string
		logCreateGroup     string
		logCreateStream    string
		logNonBlocking     string
		forceFlushInterval string
		maxBufferedEvents  string
		datetimeFormat     string
		multilinePattern   string
		shouldErr          bool
		testName           string
	}{
		{"", groupName, "", "", "", "", "", "", "", false, "defaults"},
		{"", groupName, "invalid create group", "", "", "", "", "", "", true, "invalid create group"},
		{"", groupName, "", "", "", "invalid flush interval", "", "", "", true, "invalid flush interval"},
		{"", groupName, "", "", "", "", "invalid max buffered events", "", "", true, "invalid max buffered events"},
		{"", groupName, "", "", "", "", "", "", "n{1001}", true, "invalid multiline pattern"},
		{"", groupName, "", "", "", "15", "", "", "", false, "flush interval at 15"},
		{"", groupName, "", "", "", "", "1024", "", "", false, "max buffered events at 1024"},
	}

	for _, tc := range tests {
		t.Run(tc.testName, func(t *testing.T) {
			cfg := map[string]string{
				logGroupKey:           tc.logGroupName,
				logCreateGroupKey:     tc.logCreateGroup,
				"mode":                tc.logNonBlocking,
				forceFlushIntervalKey: tc.forceFlushInterval,
				maxBufferedEventsKey:  tc.maxBufferedEvents,
				logStreamKey:          tc.logStreamName,
				logCreateStreamKey:    tc.logCreateStream,
				datetimeFormatKey:     tc.datetimeFormat,
				multilinePatternKey:   tc.multilinePattern,
			}

			info := logger.Info{
				Config: cfg,
			}
			logStreamConfig, err := newStreamConfig(info)
			if tc.shouldErr {
				assert.Check(t, err != nil, "Expected an error")
			} else {
				assert.Check(t, err == nil, "Unexpected error")
				assert.Check(t, logStreamConfig.logGroupName == tc.logGroupName, "Unexpected logGroupName")
				if tc.forceFlushInterval != "" {
					forceFlushIntervalAsInt, _ := strconv.Atoi(info.Config[forceFlushIntervalKey])
					assert.Check(t, logStreamConfig.forceFlushInterval == time.Duration(forceFlushIntervalAsInt)*time.Second, "Unexpected forceFlushInterval")
				}
				if tc.maxBufferedEvents != "" {
					maxBufferedEvents, _ := strconv.Atoi(info.Config[maxBufferedEventsKey])
					assert.Check(t, logStreamConfig.maxBufferedEvents == maxBufferedEvents, "Unexpected maxBufferedEvents")
				}
			}
		})
	}
}

func TestNewAWSLogsClientUserAgentHandler(t *testing.T) {
	info := logger.Info{
		Config: map[string]string{
			regionKey: "us-east-1",
		},
	}

	client, err := newAWSLogsClient(info)
	assert.NilError(t, err)

	realClient, ok := client.(*cloudwatchlogs.CloudWatchLogs)
	assert.Check(t, ok, "Could not cast client to cloudwatchlogs.CloudWatchLogs")

	buildHandlerList := realClient.Handlers.Build
	request := &request.Request{
		HTTPRequest: &http.Request{
			Header: http.Header{},
		},
	}
	buildHandlerList.Run(request)
	expectedUserAgentString := fmt.Sprintf("Docker %s (%s) %s/%s (%s; %s; %s)",
		dockerversion.Version, runtime.GOOS, aws.SDKName, aws.SDKVersion, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	userAgent := request.HTTPRequest.Header.Get("User-Agent")
	if userAgent != expectedUserAgentString {
		t.Errorf("Wrong User-Agent string, expected \"%s\" but was \"%s\"",
			expectedUserAgentString, userAgent)
	}
}

func TestNewAWSLogsClientAWSLogsEndpoint(t *testing.T) {
	endpoint := "mock-endpoint"
	info := logger.Info{
		Config: map[string]string{
			regionKey:   "us-east-1",
			endpointKey: endpoint,
		},
	}

	client, err := newAWSLogsClient(info)
	assert.NilError(t, err)

	realClient, ok := client.(*cloudwatchlogs.CloudWatchLogs)
	assert.Check(t, ok, "Could not cast client to cloudwatchlogs.CloudWatchLogs")

	endpointWithScheme := realClient.Endpoint
	expectedEndpointWithScheme := "https://" + endpoint
	assert.Equal(t, endpointWithScheme, expectedEndpointWithScheme, "Wrong endpoint")
}

func TestNewAWSLogsClientRegionDetect(t *testing.T) {
	info := logger.Info{
		Config: map[string]string{},
	}

	mockMetadata := newMockMetadataClient()
	newRegionFinder = func() (regionFinder, error) {
		return mockMetadata, nil
	}
	mockMetadata.regionResult <- &regionResult{
		successResult: "us-east-1",
	}

	_, err := newAWSLogsClient(info)
	assert.NilError(t, err)
}

func TestCreateSuccess(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:          mockClient,
		logGroupName:    groupName,
		logStreamName:   streamName,
		logCreateStream: true,
	}
	mockClient.createLogStreamResult <- &createLogStreamResult{}

	err := stream.create()

	if err != nil {
		t.Errorf("Received unexpected err: %v\n", err)
	}
	argument := <-mockClient.createLogStreamArgument
	if argument.LogGroupName == nil {
		t.Fatal("Expected non-nil LogGroupName")
	}
	if *argument.LogGroupName != groupName {
		t.Errorf("Expected LogGroupName to be %s", groupName)
	}
	if argument.LogStreamName == nil {
		t.Fatal("Expected non-nil LogStreamName")
	}
	if *argument.LogStreamName != streamName {
		t.Errorf("Expected LogStreamName to be %s", streamName)
	}
}

func TestCreateStreamSkipped(t *testing.T) {
	stream := &logStream{
		logGroupName:    groupName,
		logStreamName:   streamName,
		logCreateStream: false,
	}

	err := stream.create()

	if err != nil {
		t.Errorf("Received unexpected err: %v\n", err)
	}
}

func TestCreateLogGroupSuccess(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:          mockClient,
		logGroupName:    groupName,
		logStreamName:   streamName,
		logCreateGroup:  true,
		logCreateStream: true,
	}
	mockClient.createLogGroupResult <- &createLogGroupResult{}
	mockClient.createLogStreamResult <- &createLogStreamResult{}

	err := stream.create()

	if err != nil {
		t.Errorf("Received unexpected err: %v\n", err)
	}
	argument := <-mockClient.createLogStreamArgument
	if argument.LogGroupName == nil {
		t.Fatal("Expected non-nil LogGroupName")
	}
	if *argument.LogGroupName != groupName {
		t.Errorf("Expected LogGroupName to be %s", groupName)
	}
	if argument.LogStreamName == nil {
		t.Fatal("Expected non-nil LogStreamName")
	}
	if *argument.LogStreamName != streamName {
		t.Errorf("Expected LogStreamName to be %s", streamName)
	}
}

func TestCreateError(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:          mockClient,
		logCreateStream: true,
	}
	mockClient.createLogStreamResult <- &createLogStreamResult{
		errorResult: errors.New("Error"),
	}

	err := stream.create()

	if err == nil {
		t.Fatal("Expected non-nil err")
	}
}

func TestCreateAlreadyExists(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:          mockClient,
		logCreateStream: true,
	}
	mockClient.createLogStreamResult <- &createLogStreamResult{
		errorResult: awserr.New(resourceAlreadyExistsCode, "", nil),
	}

	err := stream.create()

	assert.NilError(t, err)
}

func TestLogClosed(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client: mockClient,
		closed: true,
	}
	err := stream.Log(&logger.Message{})
	if err == nil {
		t.Fatal("Expected non-nil error")
	}
}

// TestLogBlocking tests that the Log method blocks appropriately when
// non-blocking behavior is not enabled.  Blocking is achieved through an
// internal channel that must be drained for Log to return.
func TestLogBlocking(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:   mockClient,
		messages: make(chan *logger.Message),
	}

	errorCh := make(chan error, 1)
	started := make(chan bool)
	go func() {
		started <- true
		err := stream.Log(&logger.Message{})
		errorCh <- err
	}()
	// block until the goroutine above has started
	<-started
	select {
	case err := <-errorCh:
		t.Fatal("Expected stream.Log to block: ", err)
	default:
	}
	// assuming it is blocked, we can now try to drain the internal channel and
	// unblock it
	select {
	case <-time.After(10 * time.Millisecond):
		// if we're unable to drain the channel within 10ms, something seems broken
		t.Fatal("Expected to be able to read from stream.messages but was unable to")
	case <-stream.messages:
	}
	select {
	case err := <-errorCh:
		assert.NilError(t, err)

	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for read")
	}
}

func TestLogNonBlockingBufferEmpty(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:         mockClient,
		messages:       make(chan *logger.Message, 1),
		logNonBlocking: true,
	}
	err := stream.Log(&logger.Message{})
	assert.NilError(t, err)
}

func TestLogNonBlockingBufferFull(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:         mockClient,
		messages:       make(chan *logger.Message, 1),
		logNonBlocking: true,
	}
	stream.messages <- &logger.Message{}
	errorCh := make(chan error, 1)
	started := make(chan bool)
	go func() {
		started <- true
		err := stream.Log(&logger.Message{})
		errorCh <- err
	}()
	<-started
	select {
	case err := <-errorCh:
		if err == nil {
			t.Fatal("Expected non-nil error")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Expected Log call to not block")
	}
}
func TestPublishBatchSuccess(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		successResult: &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		},
	}
	events := []wrappedEvent{
		{
			inputLogEvent: &cloudwatchlogs.InputLogEvent{
				Message: aws.String(logline),
			},
		},
	}

	stream.publishBatch(testEventBatch(events))
	if stream.sequenceToken == nil {
		t.Fatal("Expected non-nil sequenceToken")
	}
	if *stream.sequenceToken != nextSequenceToken {
		t.Errorf("Expected sequenceToken to be %s, but was %s", nextSequenceToken, *stream.sequenceToken)
	}
	argument := <-mockClient.putLogEventsArgument
	if argument == nil {
		t.Fatal("Expected non-nil PutLogEventsInput")
	}
	if argument.SequenceToken == nil {
		t.Fatal("Expected non-nil PutLogEventsInput.SequenceToken")
	}
	if *argument.SequenceToken != sequenceToken {
		t.Errorf("Expected PutLogEventsInput.SequenceToken to be %s, but was %s", sequenceToken, *argument.SequenceToken)
	}
	if len(argument.LogEvents) != 1 {
		t.Errorf("Expected LogEvents to contain 1 element, but contains %d", len(argument.LogEvents))
	}
	if argument.LogEvents[0] != events[0].inputLogEvent {
		t.Error("Expected event to equal input")
	}
}

func TestPublishBatchError(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		errorResult: errors.New("Error"),
	}

	events := []wrappedEvent{
		{
			inputLogEvent: &cloudwatchlogs.InputLogEvent{
				Message: aws.String(logline),
			},
		},
	}

	stream.publishBatch(testEventBatch(events))
	if stream.sequenceToken == nil {
		t.Fatal("Expected non-nil sequenceToken")
	}
	if *stream.sequenceToken != sequenceToken {
		t.Errorf("Expected sequenceToken to be %s, but was %s", sequenceToken, *stream.sequenceToken)
	}
}

func TestPublishBatchInvalidSeqSuccess(t *testing.T) {
	mockClient := newMockClientBuffered(2)
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		errorResult: awserr.New(invalidSequenceTokenCode, "use token token", nil),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		successResult: &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		},
	}

	events := []wrappedEvent{
		{
			inputLogEvent: &cloudwatchlogs.InputLogEvent{
				Message: aws.String(logline),
			},
		},
	}

	stream.publishBatch(testEventBatch(events))
	if stream.sequenceToken == nil {
		t.Fatal("Expected non-nil sequenceToken")
	}
	if *stream.sequenceToken != nextSequenceToken {
		t.Errorf("Expected sequenceToken to be %s, but was %s", nextSequenceToken, *stream.sequenceToken)
	}

	argument := <-mockClient.putLogEventsArgument
	if argument == nil {
		t.Fatal("Expected non-nil PutLogEventsInput")
	}
	if argument.SequenceToken == nil {
		t.Fatal("Expected non-nil PutLogEventsInput.SequenceToken")
	}
	if *argument.SequenceToken != sequenceToken {
		t.Errorf("Expected PutLogEventsInput.SequenceToken to be %s, but was %s", sequenceToken, *argument.SequenceToken)
	}
	if len(argument.LogEvents) != 1 {
		t.Errorf("Expected LogEvents to contain 1 element, but contains %d", len(argument.LogEvents))
	}
	if argument.LogEvents[0] != events[0].inputLogEvent {
		t.Error("Expected event to equal input")
	}

	argument = <-mockClient.putLogEventsArgument
	if argument == nil {
		t.Fatal("Expected non-nil PutLogEventsInput")
	}
	if argument.SequenceToken == nil {
		t.Fatal("Expected non-nil PutLogEventsInput.SequenceToken")
	}
	if *argument.SequenceToken != "token" {
		t.Errorf("Expected PutLogEventsInput.SequenceToken to be %s, but was %s", "token", *argument.SequenceToken)
	}
	if len(argument.LogEvents) != 1 {
		t.Errorf("Expected LogEvents to contain 1 element, but contains %d", len(argument.LogEvents))
	}
	if argument.LogEvents[0] != events[0].inputLogEvent {
		t.Error("Expected event to equal input")
	}
}

func TestPublishBatchAlreadyAccepted(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		errorResult: awserr.New(dataAlreadyAcceptedCode, "use token token", nil),
	}

	events := []wrappedEvent{
		{
			inputLogEvent: &cloudwatchlogs.InputLogEvent{
				Message: aws.String(logline),
			},
		},
	}

	stream.publishBatch(testEventBatch(events))
	if stream.sequenceToken == nil {
		t.Fatal("Expected non-nil sequenceToken")
	}
	if *stream.sequenceToken != "token" {
		t.Errorf("Expected sequenceToken to be %s, but was %s", "token", *stream.sequenceToken)
	}

	argument := <-mockClient.putLogEventsArgument
	if argument == nil {
		t.Fatal("Expected non-nil PutLogEventsInput")
	}
	if argument.SequenceToken == nil {
		t.Fatal("Expected non-nil PutLogEventsInput.SequenceToken")
	}
	if *argument.SequenceToken != sequenceToken {
		t.Errorf("Expected PutLogEventsInput.SequenceToken to be %s, but was %s", sequenceToken, *argument.SequenceToken)
	}
	if len(argument.LogEvents) != 1 {
		t.Errorf("Expected LogEvents to contain 1 element, but contains %d", len(argument.LogEvents))
	}
	if argument.LogEvents[0] != events[0].inputLogEvent {
		t.Error("Expected event to equal input")
	}
}

func TestCollectBatchSimple(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      make(chan *logger.Message),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		successResult: &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		},
	}
	ticks := make(chan time.Time)
	newTicker = func(_ time.Duration) *time.Ticker {
		return &time.Ticker{
			C: ticks,
		}
	}
	d := make(chan bool)
	close(d)
	go stream.collectBatch(d)

	stream.Log(&logger.Message{
		Line:      []byte(logline),
		Timestamp: time.Time{},
	})

	ticks <- time.Time{}
	stream.Close()

	argument := <-mockClient.putLogEventsArgument
	if argument == nil {
		t.Fatal("Expected non-nil PutLogEventsInput")
	}
	if len(argument.LogEvents) != 1 {
		t.Errorf("Expected LogEvents to contain 1 element, but contains %d", len(argument.LogEvents))
	}
	if *argument.LogEvents[0].Message != logline {
		t.Errorf("Expected message to be %s but was %s", logline, *argument.LogEvents[0].Message)
	}
}

func TestCollectBatchTicker(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      make(chan *logger.Message),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		successResult: &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		},
	}
	ticks := make(chan time.Time)
	newTicker = func(_ time.Duration) *time.Ticker {
		return &time.Ticker{
			C: ticks,
		}
	}

	d := make(chan bool)
	close(d)
	go stream.collectBatch(d)

	stream.Log(&logger.Message{
		Line:      []byte(logline + " 1"),
		Timestamp: time.Time{},
	})
	stream.Log(&logger.Message{
		Line:      []byte(logline + " 2"),
		Timestamp: time.Time{},
	})

	ticks <- time.Time{}

	// Verify first batch
	argument := <-mockClient.putLogEventsArgument
	if argument == nil {
		t.Fatal("Expected non-nil PutLogEventsInput")
	}
	if len(argument.LogEvents) != 2 {
		t.Errorf("Expected LogEvents to contain 2 elements, but contains %d", len(argument.LogEvents))
	}
	if *argument.LogEvents[0].Message != logline+" 1" {
		t.Errorf("Expected message to be %s but was %s", logline+" 1", *argument.LogEvents[0].Message)
	}
	if *argument.LogEvents[1].Message != logline+" 2" {
		t.Errorf("Expected message to be %s but was %s", logline+" 2", *argument.LogEvents[0].Message)
	}

	stream.Log(&logger.Message{
		Line:      []byte(logline + " 3"),
		Timestamp: time.Time{},
	})

	ticks <- time.Time{}
	argument = <-mockClient.putLogEventsArgument
	if argument == nil {
		t.Fatal("Expected non-nil PutLogEventsInput")
	}
	if len(argument.LogEvents) != 1 {
		t.Errorf("Expected LogEvents to contain 1 elements, but contains %d", len(argument.LogEvents))
	}
	if *argument.LogEvents[0].Message != logline+" 3" {
		t.Errorf("Expected message to be %s but was %s", logline+" 3", *argument.LogEvents[0].Message)
	}

	stream.Close()

}

func TestCollectBatchMultilinePattern(t *testing.T) {
	mockClient := newMockClient()
	multilinePattern := regexp.MustCompile("xxxx")
	stream := &logStream{
		client:           mockClient,
		logGroupName:     groupName,
		logStreamName:    streamName,
		multilinePattern: multilinePattern,
		sequenceToken:    aws.String(sequenceToken),
		messages:         make(chan *logger.Message),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		successResult: &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		},
	}
	ticks := make(chan time.Time)
	newTicker = func(_ time.Duration) *time.Ticker {
		return &time.Ticker{
			C: ticks,
		}
	}

	d := make(chan bool)
	close(d)
	go stream.collectBatch(d)

	stream.Log(&logger.Message{
		Line:      []byte(logline),
		Timestamp: time.Now(),
	})
	stream.Log(&logger.Message{
		Line:      []byte(logline),
		Timestamp: time.Now(),
	})
	stream.Log(&logger.Message{
		Line:      []byte("xxxx " + logline),
		Timestamp: time.Now(),
	})

	ticks <- time.Now()

	// Verify single multiline event
	argument := <-mockClient.putLogEventsArgument
	assert.Check(t, argument != nil, "Expected non-nil PutLogEventsInput")
	assert.Check(t, is.Equal(1, len(argument.LogEvents)), "Expected single multiline event")
	assert.Check(t, is.Equal(logline+"\n"+logline+"\n", *argument.LogEvents[0].Message), "Received incorrect multiline message")

	stream.Close()

	// Verify single event
	argument = <-mockClient.putLogEventsArgument
	assert.Check(t, argument != nil, "Expected non-nil PutLogEventsInput")
	assert.Check(t, is.Equal(1, len(argument.LogEvents)), "Expected single multiline event")
	assert.Check(t, is.Equal("xxxx "+logline+"\n", *argument.LogEvents[0].Message), "Received incorrect multiline message")
}

func BenchmarkCollectBatch(b *testing.B) {
	for i := 0; i < b.N; i++ {
		mockClient := newMockClient()
		stream := &logStream{
			client:        mockClient,
			logGroupName:  groupName,
			logStreamName: streamName,
			sequenceToken: aws.String(sequenceToken),
			messages:      make(chan *logger.Message),
		}
		mockClient.putLogEventsResult <- &putLogEventsResult{
			successResult: &cloudwatchlogs.PutLogEventsOutput{
				NextSequenceToken: aws.String(nextSequenceToken),
			},
		}
		ticks := make(chan time.Time)
		newTicker = func(_ time.Duration) *time.Ticker {
			return &time.Ticker{
				C: ticks,
			}
		}

		d := make(chan bool)
		close(d)
		go stream.collectBatch(d)
		stream.logGenerator(10, 100)
		ticks <- time.Time{}
		stream.Close()
	}
}

func BenchmarkCollectBatchMultilinePattern(b *testing.B) {
	for i := 0; i < b.N; i++ {
		mockClient := newMockClient()
		multilinePattern := regexp.MustCompile(`\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[1,2][0-9]|3[0,1]) (?:[0,1][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]`)
		stream := &logStream{
			client:           mockClient,
			logGroupName:     groupName,
			logStreamName:    streamName,
			multilinePattern: multilinePattern,
			sequenceToken:    aws.String(sequenceToken),
			messages:         make(chan *logger.Message),
		}
		mockClient.putLogEventsResult <- &putLogEventsResult{
			successResult: &cloudwatchlogs.PutLogEventsOutput{
				NextSequenceToken: aws.String(nextSequenceToken),
			},
		}
		ticks := make(chan time.Time)
		newTicker = func(_ time.Duration) *time.Ticker {
			return &time.Ticker{
				C: ticks,
			}
		}
		d := make(chan bool)
		close(d)
		go stream.collectBatch(d)
		stream.logGenerator(10, 100)
		ticks <- time.Time{}
		stream.Close()
	}
}

func TestCollectBatchMultilinePatternMaxEventAge(t *testing.T) {
	mockClient := newMockClient()
	multilinePattern := regexp.MustCompile("xxxx")
	stream := &logStream{
		client:           mockClient,
		logGroupName:     groupName,
		logStreamName:    streamName,
		multilinePattern: multilinePattern,
		sequenceToken:    aws.String(sequenceToken),
		messages:         make(chan *logger.Message),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		successResult: &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		},
	}
	ticks := make(chan time.Time)
	newTicker = func(_ time.Duration) *time.Ticker {
		return &time.Ticker{
			C: ticks,
		}
	}

	d := make(chan bool)
	close(d)
	go stream.collectBatch(d)

	stream.Log(&logger.Message{
		Line:      []byte(logline),
		Timestamp: time.Now(),
	})

	// Log an event 1 second later
	stream.Log(&logger.Message{
		Line:      []byte(logline),
		Timestamp: time.Now().Add(time.Second),
	})

	// Fire ticker defaultForceFlushInterval seconds later
	ticks <- time.Now().Add(defaultForceFlushInterval + time.Second)

	// Verify single multiline event is flushed after maximum event buffer age (defaultForceFlushInterval)
	argument := <-mockClient.putLogEventsArgument
	assert.Check(t, argument != nil, "Expected non-nil PutLogEventsInput")
	assert.Check(t, is.Equal(1, len(argument.LogEvents)), "Expected single multiline event")
	assert.Check(t, is.Equal(logline+"\n"+logline+"\n", *argument.LogEvents[0].Message), "Received incorrect multiline message")

	// Log an event 1 second later
	stream.Log(&logger.Message{
		Line:      []byte(logline),
		Timestamp: time.Now().Add(time.Second),
	})

	// Fire ticker another defaultForceFlushInterval seconds later
	ticks <- time.Now().Add(2*defaultForceFlushInterval + time.Second)

	// Verify the event buffer is truly flushed - we should only receive a single event
	argument = <-mockClient.putLogEventsArgument
	assert.Check(t, argument != nil, "Expected non-nil PutLogEventsInput")
	assert.Check(t, is.Equal(1, len(argument.LogEvents)), "Expected single multiline event")
	assert.Check(t, is.Equal(logline+"\n", *argument.LogEvents[0].Message), "Received incorrect multiline message")
	stream.Close()
}

func TestCollectBatchMultilinePatternNegativeEventAge(t *testing.T) {
	mockClient := newMockClient()
	multilinePattern := regexp.MustCompile("xxxx")
	stream := &logStream{
		client:           mockClient,
		logGroupName:     groupName,
		logStreamName:    streamName,
		multilinePattern: multilinePattern,
		sequenceToken:    aws.String(sequenceToken),
		messages:         make(chan *logger.Message),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		successResult: &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		},
	}
	ticks := make(chan time.Time)
	newTicker = func(_ time.Duration) *time.Ticker {
		return &time.Ticker{
			C: ticks,
		}
	}

	d := make(chan bool)
	close(d)
	go stream.collectBatch(d)

	stream.Log(&logger.Message{
		Line:      []byte(logline),
		Timestamp: time.Now(),
	})

	// Log an event 1 second later
	stream.Log(&logger.Message{
		Line:      []byte(logline),
		Timestamp: time.Now().Add(time.Second),
	})

	// Fire ticker in past to simulate negative event buffer age
	ticks <- time.Now().Add(-time.Second)

	// Verify single multiline event is flushed with a negative event buffer age
	argument := <-mockClient.putLogEventsArgument
	assert.Check(t, argument != nil, "Expected non-nil PutLogEventsInput")
	assert.Check(t, is.Equal(1, len(argument.LogEvents)), "Expected single multiline event")
	assert.Check(t, is.Equal(logline+"\n"+logline+"\n", *argument.LogEvents[0].Message), "Received incorrect multiline message")

	stream.Close()
}

func TestCollectBatchMultilinePatternMaxEventSize(t *testing.T) {
	mockClient := newMockClient()
	multilinePattern := regexp.MustCompile("xxxx")
	stream := &logStream{
		client:           mockClient,
		logGroupName:     groupName,
		logStreamName:    streamName,
		multilinePattern: multilinePattern,
		sequenceToken:    aws.String(sequenceToken),
		messages:         make(chan *logger.Message),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		successResult: &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		},
	}
	ticks := make(chan time.Time)
	newTicker = func(_ time.Duration) *time.Ticker {
		return &time.Ticker{
			C: ticks,
		}
	}

	d := make(chan bool)
	close(d)
	go stream.collectBatch(d)

	// Log max event size
	longline := strings.Repeat("A", maximumBytesPerEvent)
	stream.Log(&logger.Message{
		Line:      []byte(longline),
		Timestamp: time.Now(),
	})

	// Log short event
	shortline := strings.Repeat("B", 100)
	stream.Log(&logger.Message{
		Line:      []byte(shortline),
		Timestamp: time.Now(),
	})

	// Fire ticker
	ticks <- time.Now().Add(defaultForceFlushInterval)

	// Verify multiline events
	// We expect a maximum sized event with no new line characters and a
	// second short event with a new line character at the end
	argument := <-mockClient.putLogEventsArgument
	assert.Check(t, argument != nil, "Expected non-nil PutLogEventsInput")
	assert.Check(t, is.Equal(2, len(argument.LogEvents)), "Expected two events")
	assert.Check(t, is.Equal(longline, *argument.LogEvents[0].Message), "Received incorrect multiline message")
	assert.Check(t, is.Equal(shortline+"\n", *argument.LogEvents[1].Message), "Received incorrect multiline message")
	stream.Close()
}

func TestCollectBatchClose(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      make(chan *logger.Message),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		successResult: &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		},
	}
	var ticks = make(chan time.Time)
	newTicker = func(_ time.Duration) *time.Ticker {
		return &time.Ticker{
			C: ticks,
		}
	}

	d := make(chan bool)
	close(d)
	go stream.collectBatch(d)

	stream.Log(&logger.Message{
		Line:      []byte(logline),
		Timestamp: time.Time{},
	})

	// no ticks
	stream.Close()

	argument := <-mockClient.putLogEventsArgument
	if argument == nil {
		t.Fatal("Expected non-nil PutLogEventsInput")
	}
	if len(argument.LogEvents) != 1 {
		t.Errorf("Expected LogEvents to contain 1 element, but contains %d", len(argument.LogEvents))
	}
	if *argument.LogEvents[0].Message != logline {
		t.Errorf("Expected message to be %s but was %s", logline, *argument.LogEvents[0].Message)
	}
}

func TestEffectiveLen(t *testing.T) {
	tests := []struct {
		str            string
		effectiveBytes int
	}{
		{"Hello", 5},
		{string([]byte{1, 2, 3, 4}), 4},
		{"🙃", 4},
		{string([]byte{0xFF, 0xFF, 0xFF, 0xFF}), 12},
		{"He\xff\xffo", 9},
		{"", 0},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d/%s", i, tc.str), func(t *testing.T) {
			assert.Equal(t, tc.effectiveBytes, effectiveLen(tc.str))
		})
	}
}

func TestFindValidSplit(t *testing.T) {
	tests := []struct {
		str               string
		maxEffectiveBytes int
		splitOffset       int
		effectiveBytes    int
	}{
		{"", 10, 0, 0},
		{"Hello", 6, 5, 5},
		{"Hello", 2, 2, 2},
		{"Hello", 0, 0, 0},
		{"🙃", 3, 0, 0},
		{"🙃", 4, 4, 4},
		{string([]byte{'a', 0xFF}), 2, 1, 1},
		{string([]byte{'a', 0xFF}), 4, 2, 4},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d/%s", i, tc.str), func(t *testing.T) {
			splitOffset, effectiveBytes := findValidSplit(tc.str, tc.maxEffectiveBytes)
			assert.Equal(t, tc.splitOffset, splitOffset, "splitOffset")
			assert.Equal(t, tc.effectiveBytes, effectiveBytes, "effectiveBytes")
			t.Log(tc.str[:tc.splitOffset])
			t.Log(tc.str[tc.splitOffset:])
		})
	}
}

func TestProcessEventEmoji(t *testing.T) {
	stream := &logStream{}
	batch := &eventBatch{}
	bytes := []byte(strings.Repeat("🙃", maximumBytesPerEvent/4+1))
	stream.processEvent(batch, bytes, 0)
	assert.Equal(t, 2, len(batch.batch), "should be two events in the batch")
	assert.Equal(t, strings.Repeat("🙃", maximumBytesPerEvent/4), aws.StringValue(batch.batch[0].inputLogEvent.Message))
	assert.Equal(t, "🙃", aws.StringValue(batch.batch[1].inputLogEvent.Message))
}

func TestCollectBatchLineSplit(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      make(chan *logger.Message),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		successResult: &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		},
	}
	var ticks = make(chan time.Time)
	newTicker = func(_ time.Duration) *time.Ticker {
		return &time.Ticker{
			C: ticks,
		}
	}

	d := make(chan bool)
	close(d)
	go stream.collectBatch(d)

	longline := strings.Repeat("A", maximumBytesPerEvent)
	stream.Log(&logger.Message{
		Line:      []byte(longline + "B"),
		Timestamp: time.Time{},
	})

	// no ticks
	stream.Close()

	argument := <-mockClient.putLogEventsArgument
	if argument == nil {
		t.Fatal("Expected non-nil PutLogEventsInput")
	}
	if len(argument.LogEvents) != 2 {
		t.Errorf("Expected LogEvents to contain 2 elements, but contains %d", len(argument.LogEvents))
	}
	if *argument.LogEvents[0].Message != longline {
		t.Errorf("Expected message to be %s but was %s", longline, *argument.LogEvents[0].Message)
	}
	if *argument.LogEvents[1].Message != "B" {
		t.Errorf("Expected message to be %s but was %s", "B", *argument.LogEvents[1].Message)
	}
}

func TestCollectBatchLineSplitWithBinary(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      make(chan *logger.Message),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		successResult: &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		},
	}
	var ticks = make(chan time.Time)
	newTicker = func(_ time.Duration) *time.Ticker {
		return &time.Ticker{
			C: ticks,
		}
	}

	d := make(chan bool)
	close(d)
	go stream.collectBatch(d)

	longline := strings.Repeat("\xFF", maximumBytesPerEvent/3) // 0xFF is counted as the 3-byte utf8.RuneError
	stream.Log(&logger.Message{
		Line:      []byte(longline + "\xFD"),
		Timestamp: time.Time{},
	})

	// no ticks
	stream.Close()

	argument := <-mockClient.putLogEventsArgument
	if argument == nil {
		t.Fatal("Expected non-nil PutLogEventsInput")
	}
	if len(argument.LogEvents) != 2 {
		t.Errorf("Expected LogEvents to contain 2 elements, but contains %d", len(argument.LogEvents))
	}
	if *argument.LogEvents[0].Message != longline {
		t.Errorf("Expected message to be %s but was %s", longline, *argument.LogEvents[0].Message)
	}
	if *argument.LogEvents[1].Message != "\xFD" {
		t.Errorf("Expected message to be %s but was %s", "\xFD", *argument.LogEvents[1].Message)
	}
}

func TestCollectBatchMaxEvents(t *testing.T) {
	mockClient := newMockClientBuffered(1)
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      make(chan *logger.Message),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		successResult: &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		},
	}
	var ticks = make(chan time.Time)
	newTicker = func(_ time.Duration) *time.Ticker {
		return &time.Ticker{
			C: ticks,
		}
	}

	d := make(chan bool)
	close(d)
	go stream.collectBatch(d)

	line := "A"
	for i := 0; i <= maximumLogEventsPerPut; i++ {
		stream.Log(&logger.Message{
			Line:      []byte(line),
			Timestamp: time.Time{},
		})
	}

	// no ticks
	stream.Close()

	argument := <-mockClient.putLogEventsArgument
	if argument == nil {
		t.Fatal("Expected non-nil PutLogEventsInput")
	}
	if len(argument.LogEvents) != maximumLogEventsPerPut {
		t.Errorf("Expected LogEvents to contain %d elements, but contains %d", maximumLogEventsPerPut, len(argument.LogEvents))
	}

	argument = <-mockClient.putLogEventsArgument
	if argument == nil {
		t.Fatal("Expected non-nil PutLogEventsInput")
	}
	if len(argument.LogEvents) != 1 {
		t.Errorf("Expected LogEvents to contain %d elements, but contains %d", 1, len(argument.LogEvents))
	}
}

func TestCollectBatchMaxTotalBytes(t *testing.T) {
	expectedPuts := 2
	mockClient := newMockClientBuffered(expectedPuts)
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      make(chan *logger.Message),
	}
	for i := 0; i < expectedPuts; i++ {
		mockClient.putLogEventsResult <- &putLogEventsResult{
			successResult: &cloudwatchlogs.PutLogEventsOutput{
				NextSequenceToken: aws.String(nextSequenceToken),
			},
		}
	}

	var ticks = make(chan time.Time)
	newTicker = func(_ time.Duration) *time.Ticker {
		return &time.Ticker{
			C: ticks,
		}
	}

	d := make(chan bool)
	close(d)
	go stream.collectBatch(d)

	numPayloads := maximumBytesPerPut / (maximumBytesPerEvent + perEventBytes)
	// maxline is the maximum line that could be submitted after
	// accounting for its overhead.
	maxline := strings.Repeat("A", maximumBytesPerPut-(perEventBytes*numPayloads))
	// This will be split and batched up to the `maximumBytesPerPut'
	// (+/- `maximumBytesPerEvent'). This /should/ be aligned, but
	// should also tolerate an offset within that range.
	stream.Log(&logger.Message{
		Line:      []byte(maxline[:len(maxline)/2]),
		Timestamp: time.Time{},
	})
	stream.Log(&logger.Message{
		Line:      []byte(maxline[len(maxline)/2:]),
		Timestamp: time.Time{},
	})
	stream.Log(&logger.Message{
		Line:      []byte("B"),
		Timestamp: time.Time{},
	})

	// no ticks, guarantee batch by size (and chan close)
	stream.Close()

	argument := <-mockClient.putLogEventsArgument
	if argument == nil {
		t.Fatal("Expected non-nil PutLogEventsInput")
	}

	// Should total to the maximum allowed bytes.
	eventBytes := 0
	for _, event := range argument.LogEvents {
		eventBytes += len(*event.Message)
	}
	eventsOverhead := len(argument.LogEvents) * perEventBytes
	payloadTotal := eventBytes + eventsOverhead
	// lowestMaxBatch allows the payload to be offset if the messages
	// don't lend themselves to align with the maximum event size.
	lowestMaxBatch := maximumBytesPerPut - maximumBytesPerEvent

	if payloadTotal > maximumBytesPerPut {
		t.Errorf("Expected <= %d bytes but was %d", maximumBytesPerPut, payloadTotal)
	}
	if payloadTotal < lowestMaxBatch {
		t.Errorf("Batch to be no less than %d but was %d", lowestMaxBatch, payloadTotal)
	}

	argument = <-mockClient.putLogEventsArgument
	if len(argument.LogEvents) != 1 {
		t.Errorf("Expected LogEvents to contain 1 elements, but contains %d", len(argument.LogEvents))
	}
	message := *argument.LogEvents[len(argument.LogEvents)-1].Message
	if message[len(message)-1:] != "B" {
		t.Errorf("Expected message to be %s but was %s", "B", message[len(message)-1:])
	}
}

func TestCollectBatchMaxTotalBytesWithBinary(t *testing.T) {
	expectedPuts := 2
	mockClient := newMockClientBuffered(expectedPuts)
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      make(chan *logger.Message),
	}
	for i := 0; i < expectedPuts; i++ {
		mockClient.putLogEventsResult <- &putLogEventsResult{
			successResult: &cloudwatchlogs.PutLogEventsOutput{
				NextSequenceToken: aws.String(nextSequenceToken),
			},
		}
	}

	var ticks = make(chan time.Time)
	newTicker = func(_ time.Duration) *time.Ticker {
		return &time.Ticker{
			C: ticks,
		}
	}

	d := make(chan bool)
	close(d)
	go stream.collectBatch(d)

	// maxline is the maximum line that could be submitted after
	// accounting for its overhead.
	maxline := strings.Repeat("\xFF", (maximumBytesPerPut-perEventBytes)/3) // 0xFF is counted as the 3-byte utf8.RuneError
	// This will be split and batched up to the `maximumBytesPerPut'
	// (+/- `maximumBytesPerEvent'). This /should/ be aligned, but
	// should also tolerate an offset within that range.
	stream.Log(&logger.Message{
		Line:      []byte(maxline),
		Timestamp: time.Time{},
	})
	stream.Log(&logger.Message{
		Line:      []byte("B"),
		Timestamp: time.Time{},
	})

	// no ticks, guarantee batch by size (and chan close)
	stream.Close()

	argument := <-mockClient.putLogEventsArgument
	if argument == nil {
		t.Fatal("Expected non-nil PutLogEventsInput")
	}

	// Should total to the maximum allowed bytes.
	eventBytes := 0
	for _, event := range argument.LogEvents {
		eventBytes += effectiveLen(*event.Message)
	}
	eventsOverhead := len(argument.LogEvents) * perEventBytes
	payloadTotal := eventBytes + eventsOverhead
	// lowestMaxBatch allows the payload to be offset if the messages
	// don't lend themselves to align with the maximum event size.
	lowestMaxBatch := maximumBytesPerPut - maximumBytesPerEvent

	if payloadTotal > maximumBytesPerPut {
		t.Errorf("Expected <= %d bytes but was %d", maximumBytesPerPut, payloadTotal)
	}
	if payloadTotal < lowestMaxBatch {
		t.Errorf("Batch to be no less than %d but was %d", lowestMaxBatch, payloadTotal)
	}

	argument = <-mockClient.putLogEventsArgument
	message := *argument.LogEvents[len(argument.LogEvents)-1].Message
	if message[len(message)-1:] != "B" {
		t.Errorf("Expected message to be %s but was %s", "B", message[len(message)-1:])
	}
}

func TestCollectBatchWithDuplicateTimestamps(t *testing.T) {
	mockClient := newMockClient()
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      make(chan *logger.Message),
	}
	mockClient.putLogEventsResult <- &putLogEventsResult{
		successResult: &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		},
	}
	ticks := make(chan time.Time)
	newTicker = func(_ time.Duration) *time.Ticker {
		return &time.Ticker{
			C: ticks,
		}
	}

	d := make(chan bool)
	close(d)
	go stream.collectBatch(d)

	var expectedEvents []*cloudwatchlogs.InputLogEvent
	times := maximumLogEventsPerPut
	timestamp := time.Now()
	for i := 0; i < times; i++ {
		line := fmt.Sprintf("%d", i)
		if i%2 == 0 {
			timestamp.Add(1 * time.Nanosecond)
		}
		stream.Log(&logger.Message{
			Line:      []byte(line),
			Timestamp: timestamp,
		})
		expectedEvents = append(expectedEvents, &cloudwatchlogs.InputLogEvent{
			Message:   aws.String(line),
			Timestamp: aws.Int64(timestamp.UnixNano() / int64(time.Millisecond)),
		})
	}

	ticks <- time.Time{}
	stream.Close()

	argument := <-mockClient.putLogEventsArgument
	if argument == nil {
		t.Fatal("Expected non-nil PutLogEventsInput")
	}
	if len(argument.LogEvents) != times {
		t.Errorf("Expected LogEvents to contain %d elements, but contains %d", times, len(argument.LogEvents))
	}
	for i := 0; i < times; i++ {
		if !reflect.DeepEqual(*argument.LogEvents[i], *expectedEvents[i]) {
			t.Errorf("Expected event to be %v but was %v", *expectedEvents[i], *argument.LogEvents[i])
		}
	}
}

func TestParseLogOptionsMultilinePattern(t *testing.T) {
	info := logger.Info{
		Config: map[string]string{
			multilinePatternKey: "^xxxx",
		},
	}

	multilinePattern, err := parseMultilineOptions(info)
	assert.Check(t, err, "Received unexpected error")
	assert.Check(t, multilinePattern.MatchString("xxxx"), "No multiline pattern match found")
}

func TestParseLogOptionsDatetimeFormat(t *testing.T) {
	datetimeFormatTests := []struct {
		format string
		match  string
	}{
		{"%d/%m/%y %a %H:%M:%S%L %Z", "31/12/10 Mon 08:42:44.345 NZDT"},
		{"%Y-%m-%d %A %I:%M:%S.%f%p%z", "2007-12-04 Monday 08:42:44.123456AM+1200"},
		{"%b|%b|%b|%b|%b|%b|%b|%b|%b|%b|%b|%b", "Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec"},
		{"%B|%B|%B|%B|%B|%B|%B|%B|%B|%B|%B|%B", "January|February|March|April|May|June|July|August|September|October|November|December"},
		{"%A|%A|%A|%A|%A|%A|%A", "Monday|Tuesday|Wednesday|Thursday|Friday|Saturday|Sunday"},
		{"%a|%a|%a|%a|%a|%a|%a", "Mon|Tue|Wed|Thu|Fri|Sat|Sun"},
		{"Day of the week: %w, Day of the year: %j", "Day of the week: 4, Day of the year: 091"},
	}
	for _, dt := range datetimeFormatTests {
		t.Run(dt.match, func(t *testing.T) {
			info := logger.Info{
				Config: map[string]string{
					datetimeFormatKey: dt.format,
				},
			}
			multilinePattern, err := parseMultilineOptions(info)
			assert.Check(t, err, "Received unexpected error")
			assert.Check(t, multilinePattern.MatchString(dt.match), "No multiline pattern match found")
		})
	}
}

func TestValidateLogOptionsDatetimeFormatAndMultilinePattern(t *testing.T) {
	cfg := map[string]string{
		multilinePatternKey: "^xxxx",
		datetimeFormatKey:   "%Y-%m-%d",
		logGroupKey:         groupName,
	}
	conflictingLogOptionsError := "you cannot configure log opt 'awslogs-datetime-format' and 'awslogs-multiline-pattern' at the same time"

	err := ValidateLogOpt(cfg)
	assert.Check(t, err != nil, "Expected an error")
	assert.Check(t, is.Equal(err.Error(), conflictingLogOptionsError), "Received invalid error")
}

func TestValidateLogOptionsForceFlushIntervalSeconds(t *testing.T) {
	tests := []struct {
		input     string
		shouldErr bool
	}{
		{"0", true},
		{"-1", true},
		{"a", true},
		{"10", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			cfg := map[string]string{
				forceFlushIntervalKey: tc.input,
				logGroupKey:           groupName,
			}

			err := ValidateLogOpt(cfg)
			if tc.shouldErr {
				expectedErr := "must specify a positive integer for log opt 'awslogs-force-flush-interval-seconds': " + tc.input
				assert.Error(t, err, expectedErr)
			} else {
				assert.NilError(t, err)
			}
		})
	}
}

func TestValidateLogOptionsMaxBufferedEvents(t *testing.T) {
	tests := []struct {
		input     string
		shouldErr bool
	}{
		{"0", true},
		{"-1", true},
		{"a", true},
		{"10", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			cfg := map[string]string{
				maxBufferedEventsKey: tc.input,
				logGroupKey:          groupName,
			}

			err := ValidateLogOpt(cfg)
			if tc.shouldErr {
				expectedErr := "must specify a positive integer for log opt 'awslogs-max-buffered-events': " + tc.input
				assert.Error(t, err, expectedErr)
			} else {
				assert.NilError(t, err)
			}
		})
	}
}

func TestCreateTagSuccess(t *testing.T) {
	mockClient := newMockClient()
	info := logger.Info{
		ContainerName: "/test-container",
		ContainerID:   "container-abcdefghijklmnopqrstuvwxyz01234567890",
		Config:        map[string]string{"tag": "{{.Name}}/{{.FullID}}"},
	}
	logStreamName, e := loggerutils.ParseLogTag(info, loggerutils.DefaultTemplate)
	if e != nil {
		t.Errorf("Error generating tag: %q", e)
	}
	stream := &logStream{
		client:          mockClient,
		logGroupName:    groupName,
		logStreamName:   logStreamName,
		logCreateStream: true,
	}
	mockClient.createLogStreamResult <- &createLogStreamResult{}

	err := stream.create()

	assert.NilError(t, err)
	argument := <-mockClient.createLogStreamArgument

	if *argument.LogStreamName != "test-container/container-abcdefghijklmnopqrstuvwxyz01234567890" {
		t.Errorf("Expected LogStreamName to be %s", "test-container/container-abcdefghijklmnopqrstuvwxyz01234567890")
	}
}

func BenchmarkUnwrapEvents(b *testing.B) {
	events := make([]wrappedEvent, maximumLogEventsPerPut)
	for i := 0; i < maximumLogEventsPerPut; i++ {
		mes := strings.Repeat("0", maximumBytesPerEvent)
		events[i].inputLogEvent = &cloudwatchlogs.InputLogEvent{
			Message: &mes,
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := unwrapEvents(events)
		assert.Check(b, is.Len(res, maximumLogEventsPerPut))
	}
}

func TestNewAWSLogsClientCredentialEndpointDetect(t *testing.T) {
	// required for the cloudwatchlogs client
	os.Setenv("AWS_REGION", "us-west-2")
	defer os.Unsetenv("AWS_REGION")

	credsResp := `{
		"AccessKeyId" :    "test-access-key-id",
		"SecretAccessKey": "test-secret-access-key"
		}`

	expectedAccessKeyID := "test-access-key-id"
	expectedSecretAccessKey := "test-secret-access-key"

	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, credsResp)
	}))
	defer testServer.Close()

	// set the SDKEndpoint in the driver
	newSDKEndpoint = testServer.URL

	info := logger.Info{
		Config: map[string]string{},
	}

	info.Config["awslogs-credentials-endpoint"] = "/creds"

	c, err := newAWSLogsClient(info)
	assert.Check(t, err)

	client := c.(*cloudwatchlogs.CloudWatchLogs)

	creds, err := client.Config.Credentials.Get()
	assert.Check(t, err)

	assert.Check(t, is.Equal(expectedAccessKeyID, creds.AccessKeyID))
	assert.Check(t, is.Equal(expectedSecretAccessKey, creds.SecretAccessKey))
}

func TestNewAWSLogsClientCredentialEnvironmentVariable(t *testing.T) {
	// required for the cloudwatchlogs client
	os.Setenv("AWS_REGION", "us-west-2")
	defer os.Unsetenv("AWS_REGION")

	expectedAccessKeyID := "test-access-key-id"
	expectedSecretAccessKey := "test-secret-access-key"

	os.Setenv("AWS_ACCESS_KEY_ID", expectedAccessKeyID)
	defer os.Unsetenv("AWS_ACCESS_KEY_ID")

	os.Setenv("AWS_SECRET_ACCESS_KEY", expectedSecretAccessKey)
	defer os.Unsetenv("AWS_SECRET_ACCESS_KEY")

	info := logger.Info{
		Config: map[string]string{},
	}

	c, err := newAWSLogsClient(info)
	assert.Check(t, err)

	client := c.(*cloudwatchlogs.CloudWatchLogs)

	creds, err := client.Config.Credentials.Get()
	assert.Check(t, err)

	assert.Check(t, is.Equal(expectedAccessKeyID, creds.AccessKeyID))
	assert.Check(t, is.Equal(expectedSecretAccessKey, creds.SecretAccessKey))
}

func TestNewAWSLogsClientCredentialSharedFile(t *testing.T) {
	// required for the cloudwatchlogs client
	os.Setenv("AWS_REGION", "us-west-2")
	defer os.Unsetenv("AWS_REGION")

	expectedAccessKeyID := "test-access-key-id"
	expectedSecretAccessKey := "test-secret-access-key"

	contentStr := `
	[default]
	aws_access_key_id = "test-access-key-id"
	aws_secret_access_key =  "test-secret-access-key"
	`
	content := []byte(contentStr)

	tmpfile, err := ioutil.TempFile("", "example")
	defer os.Remove(tmpfile.Name()) // clean up
	assert.Check(t, err)

	_, err = tmpfile.Write(content)
	assert.Check(t, err)

	err = tmpfile.Close()
	assert.Check(t, err)

	os.Unsetenv("AWS_ACCESS_KEY_ID")
	os.Unsetenv("AWS_SECRET_ACCESS_KEY")

	os.Setenv("AWS_SHARED_CREDENTIALS_FILE", tmpfile.Name())
	defer os.Unsetenv("AWS_SHARED_CREDENTIALS_FILE")

	info := logger.Info{
		Config: map[string]string{},
	}

	c, err := newAWSLogsClient(info)
	assert.Check(t, err)

	client := c.(*cloudwatchlogs.CloudWatchLogs)

	creds, err := client.Config.Credentials.Get()
	assert.Check(t, err)

	assert.Check(t, is.Equal(expectedAccessKeyID, creds.AccessKeyID))
	assert.Check(t, is.Equal(expectedSecretAccessKey, creds.SecretAccessKey))
}
