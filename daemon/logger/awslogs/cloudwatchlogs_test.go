package awslogs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/moby/moby/v2/daemon/logger"
	"github.com/moby/moby/v2/daemon/logger/loggerutils"
	"github.com/moby/moby/v2/dockerversion"
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
	for range multilineCount {
		l.Log(&logger.Message{
			Line:      []byte(multilineLogline),
			Timestamp: time.Time{},
		})
		for range lineCount {
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
	var userAgent string
	httpClient := smithyhttp.ClientDoFunc(func(r *http.Request) (*http.Response, error) {
		userAgent = r.Header.Get("User-Agent")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("{}")),
			Request:    r,
		}, nil
	})

	info := logger.Info{
		Config: map[string]string{
			regionKey:   "us-east-1",
			endpointKey: "http://cloudwatchlogs.test",
		},
	}

	client, err := newAWSLogsClient(
		info,
		config.WithCredentialsProvider(credentials.StaticCredentialsProvider{
			Value: aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET", SessionToken: "SESSION"},
		}),
		config.WithHTTPClient(httpClient),
	)
	assert.NilError(t, err)

	_, err = client.CreateLogGroup(t.Context(), &cloudwatchlogs.CreateLogGroupInput{LogGroupName: aws.String("foo")})
	assert.NilError(t, err)
	assert.Check(t, is.Contains(userAgent, "Docker/"+dockerversion.Version))
}

func TestNewAWSLogsClientLogFormatHeaderHandler(t *testing.T) {
	tests := []struct {
		logFormat           string
		expectedHeaderValue string
	}{
		{
			logFormat:           jsonEmfLogFormat,
			expectedHeaderValue: "json/emf",
		},
		{
			logFormat:           "",
			expectedHeaderValue: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.logFormat, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				logFormatHeaderVal := r.Header.Get("x-amzn-logs-format")
				assert.Check(t, is.Equal(tc.expectedHeaderValue, logFormatHeaderVal))
				fmt.Fprintln(w, "{}")
			}))
			defer ts.Close()

			info := logger.Info{
				Config: map[string]string{
					regionKey:    "us-east-1",
					logFormatKey: tc.logFormat,
					endpointKey:  ts.URL,
				},
			}

			client, err := newAWSLogsClient(
				info,
				config.WithCredentialsProvider(credentials.StaticCredentialsProvider{
					Value: aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET", SessionToken: "SESSION"},
				}),
			)
			assert.NilError(t, err)

			_, err = client.CreateLogGroup(t.Context(), &cloudwatchlogs.CreateLogGroupInput{LogGroupName: aws.String("foo")})
			assert.NilError(t, err)
		})
	}
}

func TestNewAWSLogsClientAWSLogsEndpoint(t *testing.T) {
	called := atomic.Value{} // for go1.19 and later, can use atomic.Bool
	called.Store(false)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		fmt.Fprintln(w, "{}")
	}))
	defer ts.Close()

	info := logger.Info{
		Config: map[string]string{
			regionKey:   "us-east-1",
			endpointKey: ts.URL,
		},
	}

	client, err := newAWSLogsClient(
		info,
		config.WithCredentialsProvider(credentials.StaticCredentialsProvider{
			Value: aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET", SessionToken: "SESSION"},
		}),
	)
	assert.NilError(t, err)

	_, err = client.CreateLogGroup(t.Context(), &cloudwatchlogs.CreateLogGroupInput{LogGroupName: aws.String("foo")})
	assert.NilError(t, err)

	// make sure the endpoint was actually hit
	assert.Check(t, called.Load().(bool))
}

func TestNewAWSLogsClientRegionDetect(t *testing.T) {
	info := logger.Info{
		Config: map[string]string{},
	}

	mockMetadata := newMockMetadataClient()
	newRegionFinder = func(context.Context) (regionFinder, error) {
		return mockMetadata, nil
	}
	mockMetadata.regionResult <- &regionResult{
		successResult: "us-east-1",
	}

	_, err := newAWSLogsClient(info)
	assert.NilError(t, err)
}

func TestCreateSuccess(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:          mockClient,
		logGroupName:    groupName,
		logStreamName:   streamName,
		logCreateStream: true,
	}
	var input *cloudwatchlogs.CreateLogStreamInput
	mockClient.createLogStreamFunc = func(ctx context.Context, i *cloudwatchlogs.CreateLogStreamInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogStreamOutput, error) {
		input = i
		return &cloudwatchlogs.CreateLogStreamOutput{}, nil
	}

	err := stream.create()

	assert.NilError(t, err)
	assert.Equal(t, groupName, aws.ToString(input.LogGroupName), "LogGroupName")
	assert.Equal(t, streamName, aws.ToString(input.LogStreamName), "LogStreamName")
}

func TestCreateStreamSkipped(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:          mockClient,
		logGroupName:    groupName,
		logStreamName:   streamName,
		logCreateStream: false,
	}
	mockClient.createLogStreamFunc = func(ctx context.Context, i *cloudwatchlogs.CreateLogStreamInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogStreamOutput, error) {
		t.Error("CreateLogStream should not be called")
		return nil, errors.New("should not be called")
	}

	err := stream.create()

	assert.NilError(t, err)
}

func TestCreateLogGroupSuccess(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:          mockClient,
		logGroupName:    groupName,
		logStreamName:   streamName,
		logCreateGroup:  true,
		logCreateStream: true,
	}
	var logGroupInput *cloudwatchlogs.CreateLogGroupInput
	mockClient.createLogGroupFunc = func(ctx context.Context, input *cloudwatchlogs.CreateLogGroupInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogGroupOutput, error) {
		logGroupInput = input
		return &cloudwatchlogs.CreateLogGroupOutput{}, nil
	}
	var logStreamInput *cloudwatchlogs.CreateLogStreamInput
	createLogStreamCalls := 0
	mockClient.createLogStreamFunc = func(ctx context.Context, input *cloudwatchlogs.CreateLogStreamInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogStreamOutput, error) {
		createLogStreamCalls++
		if logGroupInput == nil {
			// log group not created yet
			return nil, &types.ResourceNotFoundException{}
		}
		logStreamInput = input
		return &cloudwatchlogs.CreateLogStreamOutput{}, nil
	}

	err := stream.create()

	assert.NilError(t, err)
	if createLogStreamCalls < 2 {
		t.Errorf("Expected CreateLogStream to be called twice, was called %d times", createLogStreamCalls)
	}
	assert.Check(t, logGroupInput != nil)
	assert.Equal(t, groupName, aws.ToString(logGroupInput.LogGroupName), "LogGroupName in LogGroupInput")
	assert.Check(t, logStreamInput != nil)
	assert.Equal(t, groupName, aws.ToString(logStreamInput.LogGroupName), "LogGroupName in LogStreamInput")
	assert.Equal(t, streamName, aws.ToString(logStreamInput.LogStreamName), "LogStreamName in LogStreamInput")
}

func TestCreateError(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:          mockClient,
		logCreateStream: true,
	}
	mockClient.createLogStreamFunc = func(ctx context.Context, i *cloudwatchlogs.CreateLogStreamInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogStreamOutput, error) {
		return nil, errors.New("error")
	}

	err := stream.create()

	if err == nil {
		t.Fatal("Expected non-nil err")
	}
}

func TestCreateAlreadyExists(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:          mockClient,
		logCreateStream: true,
	}
	calls := 0
	mockClient.createLogStreamFunc = func(ctx context.Context, input *cloudwatchlogs.CreateLogStreamInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogStreamOutput, error) {
		calls++
		return nil, &types.ResourceAlreadyExistsException{}
	}

	err := stream.create()

	assert.NilError(t, err)
	assert.Equal(t, 1, calls)
}

func TestLogClosed(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:   mockClient,
		messages: loggerutils.NewMessageQueue(0),
	}
	stream.Close()
	err := stream.Log(&logger.Message{})
	assert.Check(t, err != nil)
}

// TestLogBlocking tests that the Log method blocks appropriately when
// non-blocking behavior is not enabled.  Blocking is achieved through an
// internal channel that must be drained for Log to return.
func TestLogBlocking(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:   mockClient,
		messages: loggerutils.NewMessageQueue(0),
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
	<-stream.messages.Receiver()

	select {
	case err := <-errorCh:
		assert.NilError(t, err)

	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for read")
	}
}

func TestLogBufferEmpty(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:   mockClient,
		messages: loggerutils.NewMessageQueue(1),
	}
	err := stream.Log(&logger.Message{})
	assert.NilError(t, err)
}

func TestPublishBatchSuccess(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
	}
	var input *cloudwatchlogs.PutLogEventsInput
	mockClient.putLogEventsFunc = func(ctx context.Context, i *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		input = i
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
	}
	events := []wrappedEvent{
		{
			inputLogEvent: types.InputLogEvent{
				Message: aws.String(logline),
			},
		},
	}

	stream.publishBatch(testEventBatch(events))
	assert.Equal(t, nextSequenceToken, aws.ToString(stream.sequenceToken), "sequenceToken")
	assert.Assert(t, input != nil)
	assert.Equal(t, sequenceToken, aws.ToString(input.SequenceToken), "input.SequenceToken")
	assert.Assert(t, len(input.LogEvents) == 1)
	assert.Equal(t, events[0].inputLogEvent, input.LogEvents[0])
}

func TestPublishBatchError(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
	}
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		return nil, errors.New("error")
	}

	events := []wrappedEvent{
		{
			inputLogEvent: types.InputLogEvent{
				Message: aws.String(logline),
			},
		},
	}

	stream.publishBatch(testEventBatch(events))
	assert.Equal(t, sequenceToken, aws.ToString(stream.sequenceToken))
}

func TestPublishBatchInvalidSeqSuccess(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		if aws.ToString(input.SequenceToken) != "token" {
			return nil, &types.InvalidSequenceTokenException{
				ExpectedSequenceToken: aws.String("token"),
			}
		}
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
	}

	events := []wrappedEvent{
		{
			inputLogEvent: types.InputLogEvent{
				Message: aws.String(logline),
			},
		},
	}

	stream.publishBatch(testEventBatch(events))
	assert.Equal(t, nextSequenceToken, aws.ToString(stream.sequenceToken))
	assert.Assert(t, len(calls) == 2)
	argument := calls[0]
	assert.Assert(t, argument != nil)
	assert.Equal(t, sequenceToken, aws.ToString(argument.SequenceToken))
	assert.Assert(t, len(argument.LogEvents) == 1)
	assert.Equal(t, events[0].inputLogEvent, argument.LogEvents[0])

	argument = calls[1]
	assert.Assert(t, argument != nil)
	assert.Equal(t, "token", aws.ToString(argument.SequenceToken))
	assert.Assert(t, len(argument.LogEvents) == 1)
	assert.Equal(t, events[0].inputLogEvent, argument.LogEvents[0])
}

func TestPublishBatchAlreadyAccepted(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		return nil, &types.DataAlreadyAcceptedException{
			ExpectedSequenceToken: aws.String("token"),
		}
	}

	events := []wrappedEvent{
		{
			inputLogEvent: types.InputLogEvent{
				Message: aws.String(logline),
			},
		},
	}

	stream.publishBatch(testEventBatch(events))
	assert.Assert(t, stream.sequenceToken != nil)
	assert.Equal(t, "token", aws.ToString(stream.sequenceToken))
	assert.Assert(t, len(calls) == 1)
	argument := calls[0]
	assert.Assert(t, argument != nil)
	assert.Equal(t, sequenceToken, aws.ToString(argument.SequenceToken))
	assert.Assert(t, len(argument.LogEvents) == 1)
	assert.Equal(t, events[0].inputLogEvent, argument.LogEvents[0])
}

func TestPublishBatchStreamNotFound(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:          mockClient,
		logGroupName:    groupName,
		logStreamName:   streamName,
		logCreateStream: true,
		sequenceToken:   aws.String(sequenceToken),
	}
	createLogStreamCalls := 0
	mockClient.createLogStreamFunc = func(ctx context.Context, input *cloudwatchlogs.CreateLogStreamInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogStreamOutput, error) {
		createLogStreamCalls++
		return &cloudwatchlogs.CreateLogStreamOutput{}, nil
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		if createLogStreamCalls == 0 {
			return nil, &types.ResourceNotFoundException{}
		}
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
	}

	events := []wrappedEvent{
		{
			inputLogEvent: types.InputLogEvent{
				Message: aws.String(logline),
			},
		},
	}

	stream.publishBatch(testEventBatch(events))
	assert.Equal(t, 1, createLogStreamCalls)
	assert.Assert(t, is.Len(calls, 2))
	assert.Equal(t, sequenceToken, aws.ToString(calls[0].SequenceToken))
	// A newly created log stream does not have a sequence token yet.
	assert.Assert(t, calls[1].SequenceToken == nil)
	assert.Assert(t, is.Len(calls[1].LogEvents, 1))
	assert.Equal(t, events[0].inputLogEvent, calls[1].LogEvents[0])
	assert.Equal(t, nextSequenceToken, aws.ToString(stream.sequenceToken))
}

func TestPublishBatchStreamNotFoundCreateStreamDisabled(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:          mockClient,
		logGroupName:    groupName,
		logStreamName:   streamName,
		logCreateStream: false,
		sequenceToken:   aws.String(sequenceToken),
	}
	mockClient.createLogStreamFunc = func(ctx context.Context, input *cloudwatchlogs.CreateLogStreamInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogStreamOutput, error) {
		t.Error("CreateLogStream should not be called")
		return nil, errors.New("should not be called")
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		return nil, &types.ResourceNotFoundException{}
	}

	events := []wrappedEvent{
		{
			inputLogEvent: types.InputLogEvent{
				Message: aws.String(logline),
			},
		},
	}

	stream.publishBatch(testEventBatch(events))
	assert.Assert(t, is.Len(calls, 1))
	assert.Equal(t, sequenceToken, aws.ToString(stream.sequenceToken))
}

func TestPublishBatchStreamNotFoundCreateError(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:          mockClient,
		logGroupName:    groupName,
		logStreamName:   streamName,
		logCreateStream: true,
		sequenceToken:   aws.String(sequenceToken),
	}
	mockClient.createLogStreamFunc = func(ctx context.Context, input *cloudwatchlogs.CreateLogStreamInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogStreamOutput, error) {
		return nil, errors.New("error")
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		return nil, &types.ResourceNotFoundException{}
	}

	events := []wrappedEvent{
		{
			inputLogEvent: types.InputLogEvent{
				Message: aws.String(logline),
			},
		},
	}

	stream.publishBatch(testEventBatch(events))
	assert.Assert(t, is.Len(calls, 1))
	assert.Equal(t, sequenceToken, aws.ToString(stream.sequenceToken))
}

func TestCollectBatchSimple(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      loggerutils.NewMessageQueue(0),
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
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

	err := stream.Log(&logger.Message{
		Line:      []byte(logline),
		Timestamp: time.Time{},
	})
	assert.NilError(t, err)

	ticks <- time.Time{}
	ticks <- time.Time{}
	stream.Close()

	for len(calls) != 1 {
		time.Sleep(10 * time.Millisecond)
	}

	assert.Assert(t, len(calls) == 1)
	argument := calls[0]
	assert.Assert(t, argument != nil)
	assert.Assert(t, len(argument.LogEvents) == 1)
	assert.Equal(t, logline, aws.ToString(argument.LogEvents[0].Message))
}

func TestCollectBatchTicker(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      loggerutils.NewMessageQueue(0),
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	called := make(chan struct{}, 50)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		called <- struct{}{}
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
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
	<-called
	assert.Assert(t, len(calls) == 1)
	argument := calls[0]
	calls = calls[1:]
	assert.Assert(t, argument != nil)
	assert.Assert(t, len(argument.LogEvents) == 2)
	assert.Equal(t, logline+" 1", aws.ToString(argument.LogEvents[0].Message))
	assert.Equal(t, logline+" 2", aws.ToString(argument.LogEvents[1].Message))

	stream.Log(&logger.Message{
		Line:      []byte(logline + " 3"),
		Timestamp: time.Time{},
	})

	ticks <- time.Time{}
	<-called
	assert.Assert(t, len(calls) == 1)
	argument = calls[0]
	close(called)
	assert.Assert(t, argument != nil)
	assert.Assert(t, len(argument.LogEvents) == 1)
	assert.Equal(t, logline+" 3", aws.ToString(argument.LogEvents[0].Message))

	stream.Close()
}

func TestCollectBatchMultilinePattern(t *testing.T) {
	mockClient := &mockClient{}
	multilinePattern := regexp.MustCompile("xxxx")
	stream := &logStream{
		client:           mockClient,
		logGroupName:     groupName,
		logStreamName:    streamName,
		multilinePattern: multilinePattern,
		sequenceToken:    aws.String(sequenceToken),
		messages:         loggerutils.NewMessageQueue(0),
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	called := make(chan struct{}, 50)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		called <- struct{}{}
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
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
	<-called
	assert.Assert(t, len(calls) == 1)
	argument := calls[0]
	calls = calls[1:]
	assert.Check(t, argument != nil, "Expected non-nil PutLogEventsInput")
	assert.Check(t, is.Equal(1, len(argument.LogEvents)), "Expected single multiline event")
	assert.Check(t, is.Equal(logline+"\n"+logline+"\n", aws.ToString(argument.LogEvents[0].Message)), "Received incorrect multiline message")

	stream.Close()

	// Verify single event
	<-called
	assert.Assert(t, len(calls) == 1)
	argument = calls[0]
	close(called)
	assert.Check(t, argument != nil, "Expected non-nil PutLogEventsInput")
	assert.Check(t, is.Equal(1, len(argument.LogEvents)), "Expected single multiline event")
	assert.Check(t, is.Equal("xxxx "+logline+"\n", aws.ToString(argument.LogEvents[0].Message)), "Received incorrect multiline message")
}

func BenchmarkCollectBatch(b *testing.B) {
	for b.Loop() {
		mockClient := &mockClient{}
		stream := &logStream{
			client:        mockClient,
			logGroupName:  groupName,
			logStreamName: streamName,
			sequenceToken: aws.String(sequenceToken),
			messages:      loggerutils.NewMessageQueue(0),
		}
		mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
			return &cloudwatchlogs.PutLogEventsOutput{
				NextSequenceToken: aws.String(nextSequenceToken),
			}, nil
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
	for b.Loop() {
		mockClient := &mockClient{}
		multilinePattern := regexp.MustCompile(`\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[1,2][0-9]|3[0,1]) (?:[0,1][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]`)
		stream := &logStream{
			client:           mockClient,
			logGroupName:     groupName,
			logStreamName:    streamName,
			multilinePattern: multilinePattern,
			sequenceToken:    aws.String(sequenceToken),
			messages:         loggerutils.NewMessageQueue(0),
		}
		mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
			return &cloudwatchlogs.PutLogEventsOutput{
				NextSequenceToken: aws.String(nextSequenceToken),
			}, nil
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
	mockClient := &mockClient{}
	multilinePattern := regexp.MustCompile("xxxx")
	stream := &logStream{
		client:           mockClient,
		logGroupName:     groupName,
		logStreamName:    streamName,
		multilinePattern: multilinePattern,
		sequenceToken:    aws.String(sequenceToken),
		messages:         loggerutils.NewMessageQueue(0),
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	called := make(chan struct{}, 50)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		called <- struct{}{}
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
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
	<-called
	assert.Assert(t, len(calls) == 1)
	argument := calls[0]
	calls = calls[1:]
	assert.Check(t, argument != nil, "Expected non-nil PutLogEventsInput")
	assert.Check(t, is.Equal(1, len(argument.LogEvents)), "Expected single multiline event")
	assert.Check(t, is.Equal(logline+"\n"+logline+"\n", aws.ToString(argument.LogEvents[0].Message)), "Received incorrect multiline message")

	// Log an event 1 second later
	stream.Log(&logger.Message{
		Line:      []byte(logline),
		Timestamp: time.Now().Add(time.Second),
	})

	// Fire ticker another defaultForceFlushInterval seconds later
	ticks <- time.Now().Add(2*defaultForceFlushInterval + time.Second)

	// Verify the event buffer is truly flushed - we should only receive a single event
	<-called
	assert.Assert(t, len(calls) == 1)
	argument = calls[0]
	close(called)
	assert.Check(t, argument != nil, "Expected non-nil PutLogEventsInput")
	assert.Check(t, is.Equal(1, len(argument.LogEvents)), "Expected single multiline event")
	assert.Check(t, is.Equal(logline+"\n", aws.ToString(argument.LogEvents[0].Message)), "Received incorrect multiline message")
	stream.Close()
}

func TestCollectBatchMultilinePatternNegativeEventAge(t *testing.T) {
	mockClient := &mockClient{}
	multilinePattern := regexp.MustCompile("xxxx")
	stream := &logStream{
		client:           mockClient,
		logGroupName:     groupName,
		logStreamName:    streamName,
		multilinePattern: multilinePattern,
		sequenceToken:    aws.String(sequenceToken),
		messages:         loggerutils.NewMessageQueue(0),
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	called := make(chan struct{}, 50)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		called <- struct{}{}
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
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
	<-called
	assert.Assert(t, len(calls) == 1)
	argument := calls[0]
	close(called)
	assert.Check(t, argument != nil, "Expected non-nil PutLogEventsInput")
	assert.Check(t, is.Equal(1, len(argument.LogEvents)), "Expected single multiline event")
	assert.Check(t, is.Equal(logline+"\n"+logline+"\n", aws.ToString(argument.LogEvents[0].Message)), "Received incorrect multiline message")

	stream.Close()
}

func TestCollectBatchMultilinePatternMaxEventSize(t *testing.T) {
	mockClient := &mockClient{}
	multilinePattern := regexp.MustCompile("xxxx")
	stream := &logStream{
		client:           mockClient,
		logGroupName:     groupName,
		logStreamName:    streamName,
		multilinePattern: multilinePattern,
		sequenceToken:    aws.String(sequenceToken),
		messages:         loggerutils.NewMessageQueue(0),
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	called := make(chan struct{}, 50)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		called <- struct{}{}
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
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
	<-called
	assert.Assert(t, len(calls) == 1)
	argument := calls[0]
	close(called)
	assert.Check(t, argument != nil, "Expected non-nil PutLogEventsInput")
	assert.Check(t, is.Equal(2, len(argument.LogEvents)), "Expected two events")
	assert.Check(t, is.Equal(longline, aws.ToString(argument.LogEvents[0].Message)), "Received incorrect multiline message")
	assert.Check(t, is.Equal(shortline+"\n", aws.ToString(argument.LogEvents[1].Message)), "Received incorrect multiline message")
	stream.Close()
}

func TestCollectBatchClose(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      loggerutils.NewMessageQueue(0),
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	called := make(chan struct{}, 50)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		called <- struct{}{}
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
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

	// no ticks
	stream.Close()

	<-called
	assert.Assert(t, len(calls) == 1)
	argument := calls[0]
	close(called)
	assert.Assert(t, argument != nil)
	assert.Assert(t, len(argument.LogEvents) == 1)
	assert.Equal(t, logline, *(argument.LogEvents[0].Message))
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
	assert.Equal(t, strings.Repeat("🙃", maximumBytesPerEvent/4), *batch.batch[0].inputLogEvent.Message)
	assert.Equal(t, "🙃", *batch.batch[1].inputLogEvent.Message)
}

func TestCollectBatchLineSplit(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      loggerutils.NewMessageQueue(0),
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	called := make(chan struct{}, 50)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		called <- struct{}{}
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
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

	longline := strings.Repeat("A", maximumBytesPerEvent)
	stream.Log(&logger.Message{
		Line:      []byte(longline + "B"),
		Timestamp: time.Time{},
	})

	// no ticks
	stream.Close()

	<-called
	assert.Assert(t, len(calls) == 1)
	argument := calls[0]
	close(called)
	assert.Assert(t, argument != nil)
	assert.Assert(t, len(argument.LogEvents) == 2)
	assert.Equal(t, longline, aws.ToString(argument.LogEvents[0].Message))
	assert.Equal(t, "B", aws.ToString(argument.LogEvents[1].Message))
}

func TestCollectBatchLineSplitWithBinary(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      loggerutils.NewMessageQueue(0),
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	called := make(chan struct{}, 50)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		called <- struct{}{}
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
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

	longline := strings.Repeat("\xFF", maximumBytesPerEvent/3) // 0xFF is counted as the 3-byte utf8.RuneError
	stream.Log(&logger.Message{
		Line:      []byte(longline + "\xFD"),
		Timestamp: time.Time{},
	})

	// no ticks
	stream.Close()

	<-called
	assert.Assert(t, len(calls) == 1)
	argument := calls[0]
	close(called)
	assert.Assert(t, argument != nil)
	assert.Assert(t, len(argument.LogEvents) == 2)
	assert.Equal(t, longline, aws.ToString(argument.LogEvents[0].Message))
	assert.Equal(t, "\xFD", aws.ToString(argument.LogEvents[1].Message))
}

func TestCollectBatchMaxEvents(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      loggerutils.NewMessageQueue(0),
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	called := make(chan struct{}, 50)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		called <- struct{}{}
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
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

	line := "A"
	for i := 0; i <= maximumLogEventsPerPut; i++ {
		stream.Log(&logger.Message{
			Line:      []byte(line),
			Timestamp: time.Time{},
		})
	}

	// no ticks
	stream.Close()

	<-called
	<-called
	assert.Assert(t, len(calls) == 2)
	argument := calls[0]
	assert.Assert(t, argument != nil)
	assert.Check(t, len(argument.LogEvents) == maximumLogEventsPerPut)

	argument = calls[1]
	close(called)
	assert.Assert(t, argument != nil)
	assert.Assert(t, len(argument.LogEvents) == 1)
}

func TestCollectBatchMaxTotalBytes(t *testing.T) {
	expectedPuts := 2
	mockClient := &mockClient{}
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      loggerutils.NewMessageQueue(0),
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	called := make(chan struct{}, 50)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		called <- struct{}{}
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
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

	for range expectedPuts {
		<-called
	}
	assert.Assert(t, len(calls) == expectedPuts)
	argument := calls[0]
	assert.Assert(t, argument != nil)

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

	assert.Check(t, payloadTotal <= maximumBytesPerPut)
	assert.Check(t, payloadTotal >= lowestMaxBatch)

	argument = calls[1]
	assert.Assert(t, len(argument.LogEvents) == 1)
	message := *argument.LogEvents[len(argument.LogEvents)-1].Message
	assert.Equal(t, "B", message[len(message)-1:])
}

func TestCollectBatchMaxTotalBytesWithBinary(t *testing.T) {
	expectedPuts := 2
	mockClient := &mockClient{}
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      loggerutils.NewMessageQueue(0),
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	called := make(chan struct{}, 50)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		called <- struct{}{}
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
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

	for range expectedPuts {
		<-called
	}
	assert.Assert(t, len(calls) == expectedPuts)
	argument := calls[0]
	assert.Assert(t, argument != nil)

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

	assert.Check(t, payloadTotal <= maximumBytesPerPut)
	assert.Check(t, payloadTotal >= lowestMaxBatch)

	argument = calls[1]
	message := *argument.LogEvents[len(argument.LogEvents)-1].Message
	assert.Equal(t, "B", message[len(message)-1:])
}

func TestCollectBatchWithDuplicateTimestamps(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		messages:      loggerutils.NewMessageQueue(0),
	}
	calls := make([]*cloudwatchlogs.PutLogEventsInput, 0)
	called := make(chan struct{}, 50)
	mockClient.putLogEventsFunc = func(ctx context.Context, input *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		calls = append(calls, input)
		called <- struct{}{}
		return &cloudwatchlogs.PutLogEventsOutput{
			NextSequenceToken: aws.String(nextSequenceToken),
		}, nil
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

	var expectedEvents []types.InputLogEvent
	times := maximumLogEventsPerPut
	timestamp := time.Now()
	for i := range times {
		line := strconv.Itoa(i)
		if i%2 == 0 {
			timestamp = timestamp.Add(1 * time.Nanosecond)
		}
		stream.Log(&logger.Message{
			Line:      []byte(line),
			Timestamp: timestamp,
		})
		expectedEvents = append(expectedEvents, types.InputLogEvent{
			Message:   aws.String(line),
			Timestamp: aws.Int64(timestamp.UnixNano() / int64(time.Millisecond)),
		})
	}

	ticks <- time.Time{}
	stream.Close()

	<-called
	assert.Assert(t, len(calls) == 1)
	argument := calls[0]
	close(called)
	assert.Assert(t, argument != nil)
	assert.Assert(t, len(argument.LogEvents) == times)
	for i := range times {
		if !reflect.DeepEqual(argument.LogEvents[i], expectedEvents[i]) {
			t.Errorf("Expected event to be %v but was %v", expectedEvents[i], argument.LogEvents[i])
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

func TestValidateLogOptionsFormat(t *testing.T) {
	tests := []struct {
		format           string
		multiLinePattern string
		datetimeFormat   string
		expErrMsg        string
	}{
		{"json/emf", "", "", ""},
		{"random", "", "", "unsupported log format 'random'"},
		{"", "", "", ""},
		{"json/emf", "---", "", "you cannot configure log opt 'awslogs-datetime-format' or 'awslogs-multiline-pattern' when log opt 'awslogs-format' is set to 'json/emf'"},
		{"json/emf", "", "yyyy-dd-mm", "you cannot configure log opt 'awslogs-datetime-format' or 'awslogs-multiline-pattern' when log opt 'awslogs-format' is set to 'json/emf'"},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d/%s", i, tc.format), func(t *testing.T) {
			cfg := map[string]string{
				logGroupKey:  groupName,
				logFormatKey: tc.format,
			}
			if tc.multiLinePattern != "" {
				cfg[multilinePatternKey] = tc.multiLinePattern
			}
			if tc.datetimeFormat != "" {
				cfg[datetimeFormatKey] = tc.datetimeFormat
			}

			err := ValidateLogOpt(cfg)
			if tc.expErrMsg != "" {
				assert.Error(t, err, tc.expErrMsg)
			} else {
				assert.NilError(t, err)
			}
		})
	}
}

func TestValidateLogOptionsCreateLogStream(t *testing.T) {
	for _, tc := range []struct {
		createLogStream string
		shouldErr       bool
	}{
		{"true", false},
		{"false", false},
		{"", false},
		{"invalid", true},
	} {
		t.Run(tc.createLogStream, func(t *testing.T) {
			cfg := map[string]string{
				logGroupKey:        groupName,
				logCreateStreamKey: tc.createLogStream,
			}

			if err := ValidateLogOpt(cfg); tc.shouldErr {
				assert.ErrorContains(t, err, "must specify valid value for log opt 'awslogs-create-stream'")
			} else {
				assert.NilError(t, err)
			}
		})
	}
}

func TestCreateTagSuccess(t *testing.T) {
	mockClient := &mockClient{}
	info := logger.Info{
		ContainerName: "/test-container",
		ContainerID:   "container-abcdefghijklmnopqrstuvwxyz01234567890",
		Config:        map[string]string{logger.AttrLogTag: "{{.Name}}/{{.FullID}}"},
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
	calls := make([]*cloudwatchlogs.CreateLogStreamInput, 0)
	mockClient.createLogStreamFunc = func(ctx context.Context, input *cloudwatchlogs.CreateLogStreamInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogStreamOutput, error) {
		calls = append(calls, input)
		return &cloudwatchlogs.CreateLogStreamOutput{}, nil
	}

	err := stream.create()

	assert.NilError(t, err)
	assert.Equal(t, 1, len(calls))
	argument := calls[0]

	assert.Equal(t, "test-container/container-abcdefghijklmnopqrstuvwxyz01234567890", aws.ToString(argument.LogStreamName))
}

func BenchmarkUnwrapEvents(b *testing.B) {
	events := make([]wrappedEvent, maximumLogEventsPerPut)
	for i := range maximumLogEventsPerPut {
		mes := strings.Repeat("0", maximumBytesPerEvent)
		events[i].inputLogEvent = types.InputLogEvent{
			Message: &mes,
		}
	}

	for b.Loop() {
		res := unwrapEvents(events)
		assert.Check(b, is.Len(res, maximumLogEventsPerPut))
	}
}

func TestNewStreamConfigEntity(t *testing.T) {
	tests := []struct {
		testName      string
		containerName string
		config        map[string]string
		shouldErr     bool
		wantNil       bool
		wantKeyAttr   map[string]string
		wantAttr      map[string]string
	}{
		{
			testName: "no entity options",
			config:   map[string]string{logGroupKey: groupName},
			wantNil:  true,
		},
		{
			testName: "service name and environment",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: "my-service",
				entityEnvironmentKey: "prod",
			},
			wantKeyAttr: map[string]string{"Type": "Service", "Name": "my-service", "Environment": "prod"},
		},
		{
			testName:      "templated service name",
			containerName: "/test-container",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: "{{.Name}}",
				entityEnvironmentKey: "prod",
			},
			wantKeyAttr: map[string]string{"Type": "Service", "Name": "test-container", "Environment": "prod"},
		},
		{
			testName: "with attributes",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: "my-service",
				entityEnvironmentKey: "prod",
				entityAttributesKey:  "PlatformType=Generic, Host=worker-7 ,K8s.Cluster=foo=bar",
			},
			wantKeyAttr: map[string]string{"Type": "Service", "Name": "my-service", "Environment": "prod"},
			wantAttr:    map[string]string{"PlatformType": "Generic", "Host": "worker-7", "K8s.Cluster": "foo=bar"},
		},
		{
			testName: "attributes containing commas",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: "my-service",
				entityEnvironmentKey: "prod",
				entityAttributesKey:  `"Telemetry.SDK=opentelemetry,1.32.0-aws-SNAPSHOT,java,Auto","Telemetry.Source=ClientSpan, JMX"`,
			},
			wantKeyAttr: map[string]string{"Type": "Service", "Name": "my-service", "Environment": "prod"},
			wantAttr: map[string]string{
				"Telemetry.SDK":    "opentelemetry,1.32.0-aws-SNAPSHOT,java,Auto",
				"Telemetry.Source": "ClientSpan, JMX",
			},
		},
		{
			testName: "multi-byte service name within character limit",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: strings.Repeat("界", maxEntityValue),
				entityEnvironmentKey: "prod",
			},
			wantKeyAttr: map[string]string{"Type": "Service", "Name": strings.Repeat("界", maxEntityValue), "Environment": "prod"},
		},
		{
			testName: "trailing comma in attributes is tolerated",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: "my-service",
				entityEnvironmentKey: "prod",
				entityAttributesKey:  "Host=worker-7,",
			},
			wantKeyAttr: map[string]string{"Type": "Service", "Name": "my-service", "Environment": "prod"},
			wantAttr:    map[string]string{"Host": "worker-7"},
		},
		{
			testName: "service name without environment",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: "my-service",
			},
			shouldErr: true,
		},
		{
			testName: "malformed attribute pair",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: "my-service",
				entityEnvironmentKey: "prod",
				entityAttributesKey:  "PlatformType",
			},
			shouldErr: true,
		},
		{
			testName: "invalid template field in service name",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: "{{.Nope}}",
				entityEnvironmentKey: "prod",
			},
			shouldErr: true,
		},
		{
			testName: "invalid template field in environment",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: "my-service",
				entityEnvironmentKey: "{{.Nope}}",
			},
			shouldErr: true,
		},
		{
			testName: "malformed template syntax",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: "{{.Name",
				entityEnvironmentKey: "prod",
			},
			shouldErr: true,
		},
		{
			testName: "service name resolves to empty value",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: "{{.Name}}",
				entityEnvironmentKey: "prod",
			},
			// containerName is left empty, so {{.Name}} resolves to ""
			shouldErr: true,
		},
		{
			testName: "service name exceeds length limit",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: strings.Repeat("x", maxEntityValue+1),
				entityEnvironmentKey: "prod",
			},
			shouldErr: true,
		},
		{
			testName: "environment exceeds length limit",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: "my-service",
				entityEnvironmentKey: strings.Repeat("x", maxEntityValue+1),
			},
			shouldErr: true,
		},
		{
			testName: "blank-only attributes resolve to no attributes",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: "my-service",
				entityEnvironmentKey: "prod",
				entityAttributesKey:  ", ,",
			},
			wantKeyAttr: map[string]string{"Type": "Service", "Name": "my-service", "Environment": "prod"},
			wantAttr:    nil,
		},
		{
			testName: "duplicate attribute key",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: "my-service",
				entityEnvironmentKey: "prod",
				entityAttributesKey:  "Host=a,Host=b",
			},
			shouldErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.testName, func(t *testing.T) {
			info := logger.Info{
				ContainerName: tc.containerName,
				Config:        tc.config,
			}
			streamConfig, err := newStreamConfig(info)
			if tc.shouldErr {
				assert.Check(t, err != nil, "Expected an error")
				return
			}
			assert.NilError(t, err)
			if tc.wantNil {
				assert.Check(t, is.Nil(streamConfig.entity))
				return
			}
			assert.Assert(t, streamConfig.entity != nil)
			assert.DeepEqual(t, tc.wantKeyAttr, streamConfig.entity.KeyAttributes)
			assert.DeepEqual(t, tc.wantAttr, streamConfig.entity.Attributes)
		})
	}
}

func TestValidateLogOptEntity(t *testing.T) {
	tests := []struct {
		testName  string
		config    map[string]string
		shouldErr bool
	}{
		{
			testName: "both key attributes set",
			config:   map[string]string{logGroupKey: groupName, entityServiceNameKey: "svc", entityEnvironmentKey: "prod"},
		},
		{
			testName:  "only service name set",
			config:    map[string]string{logGroupKey: groupName, entityServiceNameKey: "svc"},
			shouldErr: true,
		},
		{
			testName:  "only environment set",
			config:    map[string]string{logGroupKey: groupName, entityEnvironmentKey: "prod"},
			shouldErr: true,
		},
		{
			testName: "valid attributes",
			config:   map[string]string{logGroupKey: groupName, entityServiceNameKey: "svc", entityEnvironmentKey: "prod", entityAttributesKey: "a=1,b=2"},
		},
		{
			testName:  "malformed attributes",
			config:    map[string]string{logGroupKey: groupName, entityServiceNameKey: "svc", entityEnvironmentKey: "prod", entityAttributesKey: "a"},
			shouldErr: true,
		},
		{
			testName:  "too many attributes",
			config:    map[string]string{logGroupKey: groupName, entityServiceNameKey: "svc", entityEnvironmentKey: "prod", entityAttributesKey: "a=1,b=2,c=3,d=4,e=5,f=6,g=7,h=8,i=9,j=10,k=11"},
			shouldErr: true,
		},
		{
			testName:  "attribute value too long",
			config:    map[string]string{logGroupKey: groupName, entityServiceNameKey: "svc", entityEnvironmentKey: "prod", entityAttributesKey: "a=" + strings.Repeat("x", maxEntityValue+1)},
			shouldErr: true,
		},
		{
			testName: "multi-byte attribute key and value within character limits",
			config: map[string]string{
				logGroupKey:          groupName,
				entityServiceNameKey: "svc",
				entityEnvironmentKey: "prod",
				entityAttributesKey:  strings.Repeat("界", maxAttributeKey) + "=" + strings.Repeat("界", maxEntityValue),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.testName, func(t *testing.T) {
			err := ValidateLogOpt(tc.config)
			if tc.shouldErr {
				assert.Check(t, err != nil, "Expected an error")
			} else {
				assert.NilError(t, err)
			}
		})
	}
}

func TestPublishBatchEntity(t *testing.T) {
	mockClient := &mockClient{}
	entity := &types.Entity{
		KeyAttributes: map[string]string{"Type": "Service", "Name": "my-service", "Environment": "prod"},
		Attributes:    map[string]string{"PlatformType": "Generic"},
	}
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		entity:        entity,
	}
	var input *cloudwatchlogs.PutLogEventsInput
	mockClient.putLogEventsFunc = func(ctx context.Context, i *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		input = i
		return &cloudwatchlogs.PutLogEventsOutput{NextSequenceToken: aws.String(nextSequenceToken)}, nil
	}
	events := []wrappedEvent{
		{inputLogEvent: types.InputLogEvent{Message: aws.String(logline)}},
	}

	stream.publishBatch(testEventBatch(events))
	assert.Assert(t, input != nil)
	assert.Assert(t, input.Entity != nil)
	assert.DeepEqual(t, entity.KeyAttributes, input.Entity.KeyAttributes)
	assert.DeepEqual(t, entity.Attributes, input.Entity.Attributes)
}

func TestPublishBatchEntityRejected(t *testing.T) {
	mockClient := &mockClient{}
	stream := &logStream{
		client:        mockClient,
		logGroupName:  groupName,
		logStreamName: streamName,
		sequenceToken: aws.String(sequenceToken),
		entity: &types.Entity{
			KeyAttributes: map[string]string{"Type": "Service", "Name": "my-service", "Environment": "prod"},
		},
	}
	var inputs []*cloudwatchlogs.PutLogEventsInput
	mockClient.putLogEventsFunc = func(ctx context.Context, i *cloudwatchlogs.PutLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
		inputs = append(inputs, i)
		output := &cloudwatchlogs.PutLogEventsOutput{NextSequenceToken: aws.String(nextSequenceToken)}
		if i.Entity != nil {
			output.RejectedEntityInfo = &types.RejectedEntityInfo{ErrorType: types.EntityRejectionErrorTypeInvalidEntity}
		}
		return output, nil
	}
	events := []wrappedEvent{
		{inputLogEvent: types.InputLogEvent{Message: aws.String(logline)}},
	}

	batch := testEventBatch(events)
	stream.publishBatch(batch)
	stream.publishBatch(batch)

	assert.Equal(t, nextSequenceToken, aws.ToString(stream.sequenceToken), "sequenceToken")
	assert.Equal(t, len(inputs), 2)
	assert.Assert(t, inputs[0].Entity != nil)
	assert.Check(t, is.Nil(inputs[1].Entity))
}

func TestNewAWSLogsClientCredentialEndpointDetect(t *testing.T) {
	// required for the cloudwatchlogs client
	t.Setenv("AWS_REGION", "us-west-2")

	// #nosec G101 -- ignore potential hardcoded credentials
	credsResp := `{
		"AccessKeyId" :    "test-access-key-id",
		"SecretAccessKey": "test-secret-access-key"
		}`

	credsRetrieved := false
	actualAuthHeader := ""

	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/creds":
			credsRetrieved = true
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, credsResp)
		case "/":
			actualAuthHeader = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, "{}")
		}
	}))
	defer testServer.Close()

	// set the SDKEndpoint in the driver
	newSDKEndpoint = testServer.URL

	info := logger.Info{
		Config: map[string]string{
			endpointKey:            testServer.URL,
			credentialsEndpointKey: "/creds",
		},
	}

	client, err := newAWSLogsClient(info)
	assert.Check(t, err)

	_, err = client.CreateLogGroup(t.Context(), &cloudwatchlogs.CreateLogGroupInput{LogGroupName: aws.String("foo")})
	assert.NilError(t, err)

	assert.Check(t, credsRetrieved)

	// sample header val:
	// AWS4-HMAC-SHA256 Credential=test-access-key-id/20220915/us-west-2/logs/aws4_request, SignedHeaders=amz-sdk-invocation-id;amz-sdk-request;content-length;content-type;host;x-amz-date;x-amz-target, Signature=9cc0f8347e379ec77884616bb4b5a9d4a9a11f63cdc4c765e2f0131f45fe06d3
	assert.Check(t, is.Contains(actualAuthHeader, "AWS4-HMAC-SHA256 Credential=test-access-key-id/"))
	assert.Check(t, is.Contains(actualAuthHeader, "us-west-2"))
	assert.Check(t, is.Contains(actualAuthHeader, "Signature="))
}
