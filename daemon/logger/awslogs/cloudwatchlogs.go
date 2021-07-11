// Package awslogs provides the logdriver for forwarding container logs to Amazon CloudWatch Logs
package awslogs // import "github.com/docker/docker/daemon/logger/awslogs"

import (
	"fmt"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials/endpointcreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/loggerutils"
	"github.com/docker/docker/dockerversion"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	name                   = "awslogs"
	regionKey              = "awslogs-region"
	endpointKey            = "awslogs-endpoint"
	regionEnvKey           = "AWS_REGION"
	logGroupKey            = "awslogs-group"
	logStreamKey           = "awslogs-stream"
	logCreateGroupKey      = "awslogs-create-group"
	logCreateStreamKey     = "awslogs-create-stream"
	tagKey                 = "tag"
	datetimeFormatKey      = "awslogs-datetime-format"
	multilinePatternKey    = "awslogs-multiline-pattern"
	credentialsEndpointKey = "awslogs-credentials-endpoint"
	forceFlushIntervalKey  = "awslogs-force-flush-interval-seconds"
	maxBufferedEventsKey   = "awslogs-max-buffered-events"

	defaultForceFlushInterval = 5 * time.Second
	defaultMaxBufferedEvents  = 4096

	// See: http://docs.aws.amazon.com/AmazonCloudWatchLogs/latest/APIReference/API_PutLogEvents.html
	perEventBytes          = 26
	maximumBytesPerPut     = 1048576
	maximumLogEventsPerPut = 10000

	// See: http://docs.aws.amazon.com/AmazonCloudWatch/latest/DeveloperGuide/cloudwatch_limits.html
	// Because the events are interpreted as UTF-8 encoded Unicode, invalid UTF-8 byte sequences are replaced with the
	// Unicode replacement character (U+FFFD), which is a 3-byte sequence in UTF-8.  To compensate for that and to avoid
	// splitting valid UTF-8 characters into invalid byte sequences, we calculate the length of each event assuming that
	// this replacement happens.
	maximumBytesPerEvent = 262144 - perEventBytes

	resourceAlreadyExistsCode = "ResourceAlreadyExistsException"
	dataAlreadyAcceptedCode   = "DataAlreadyAcceptedException"
	invalidSequenceTokenCode  = "InvalidSequenceTokenException"
	resourceNotFoundCode      = "ResourceNotFoundException"

	credentialsEndpoint = "http://169.254.170.2"

	userAgentHeader = "User-Agent"
)

type logStream struct {
	logStreamName      string
	logGroupName       string
	logCreateGroup     bool
	logCreateStream    bool
	logNonBlocking     bool
	forceFlushInterval time.Duration
	multilinePattern   *regexp.Regexp
	client             api
	messages           chan *logger.Message
	lock               sync.RWMutex
	closed             bool
	sequenceToken      *string
}

type logStreamConfig struct {
	logStreamName      string
	logGroupName       string
	logCreateGroup     bool
	logCreateStream    bool
	logNonBlocking     bool
	forceFlushInterval time.Duration
	maxBufferedEvents  int
	multilinePattern   *regexp.Regexp
}

var _ logger.SizedLogger = &logStream{}

type api interface {
	CreateLogGroup(*cloudwatchlogs.CreateLogGroupInput) (*cloudwatchlogs.CreateLogGroupOutput, error)
	CreateLogStream(*cloudwatchlogs.CreateLogStreamInput) (*cloudwatchlogs.CreateLogStreamOutput, error)
	PutLogEvents(*cloudwatchlogs.PutLogEventsInput) (*cloudwatchlogs.PutLogEventsOutput, error)
}

type regionFinder interface {
	Region() (string, error)
}

type wrappedEvent struct {
	inputLogEvent *cloudwatchlogs.InputLogEvent
	insertOrder   int
}
type byTimestamp []wrappedEvent

// init registers the awslogs driver
func init() {
	if err := logger.RegisterLogDriver(name, New); err != nil {
		logrus.Fatal(err)
	}
	if err := logger.RegisterLogOptValidator(name, ValidateLogOpt); err != nil {
		logrus.Fatal(err)
	}
}

// eventBatch holds the events that are batched for submission and the
// associated data about it.
//
// Warning: this type is not threadsafe and must not be used
// concurrently. This type is expected to be consumed in a single go
// routine and never concurrently.
type eventBatch struct {
	batch []wrappedEvent
	bytes int
}

// New creates an awslogs logger using the configuration passed in on the
// context.  Supported context configuration variables are awslogs-region,
// awslogs-endpoint, awslogs-group, awslogs-stream, awslogs-create-group,
// awslogs-multiline-pattern and awslogs-datetime-format.
// When available, configuration is also taken from environment variables
// AWS_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, the shared credentials
// file (~/.aws/credentials), and the EC2 Instance Metadata Service.
func New(info logger.Info) (logger.Logger, error) {
	containerStreamConfig, err := newStreamConfig(info)
	if err != nil {
		return nil, err
	}
	client, err := newAWSLogsClient(info)
	if err != nil {
		return nil, err
	}

	containerStream := &logStream{
		logStreamName:      containerStreamConfig.logStreamName,
		logGroupName:       containerStreamConfig.logGroupName,
		logCreateGroup:     containerStreamConfig.logCreateGroup,
		logCreateStream:    containerStreamConfig.logCreateStream,
		logNonBlocking:     containerStreamConfig.logNonBlocking,
		forceFlushInterval: containerStreamConfig.forceFlushInterval,
		multilinePattern:   containerStreamConfig.multilinePattern,
		client:             client,
		messages:           make(chan *logger.Message, containerStreamConfig.maxBufferedEvents),
	}

	creationDone := make(chan bool)
	if containerStream.logNonBlocking {
		go func() {
			backoff := 1
			maxBackoff := 32
			for {
				// If logger is closed we are done
				containerStream.lock.RLock()
				if containerStream.closed {
					containerStream.lock.RUnlock()
					break
				}
				containerStream.lock.RUnlock()
				err := containerStream.create()
				if err == nil {
					break
				}

				time.Sleep(time.Duration(backoff) * time.Second)
				if backoff < maxBackoff {
					backoff *= 2
				}
				logrus.
					WithError(err).
					WithField("container-id", info.ContainerID).
					WithField("container-name", info.ContainerName).
					Error("Error while trying to initialize awslogs. Retrying in: ", backoff, " seconds")
			}
			close(creationDone)
		}()
	} else {
		if err = containerStream.create(); err != nil {
			return nil, err
		}
		close(creationDone)
	}
	go containerStream.collectBatch(creationDone)

	return containerStream, nil
}

// Parses most of the awslogs- options and prepares a config object to be used for newing the actual stream
// It has been formed out to ease Utest of the New above
func newStreamConfig(info logger.Info) (*logStreamConfig, error) {
	logGroupName := info.Config[logGroupKey]
	logStreamName, err := loggerutils.ParseLogTag(info, "{{.FullID}}")
	if err != nil {
		return nil, err
	}
	logCreateGroup := false
	if info.Config[logCreateGroupKey] != "" {
		logCreateGroup, err = strconv.ParseBool(info.Config[logCreateGroupKey])
		if err != nil {
			return nil, err
		}
	}

	logNonBlocking := info.Config["mode"] == "non-blocking"

	forceFlushInterval := defaultForceFlushInterval
	if info.Config[forceFlushIntervalKey] != "" {
		forceFlushIntervalAsInt, err := strconv.Atoi(info.Config[forceFlushIntervalKey])
		if err != nil {
			return nil, err
		}
		forceFlushInterval = time.Duration(forceFlushIntervalAsInt) * time.Second
	}

	maxBufferedEvents := int(defaultMaxBufferedEvents)
	if info.Config[maxBufferedEventsKey] != "" {
		maxBufferedEvents, err = strconv.Atoi(info.Config[maxBufferedEventsKey])
		if err != nil {
			return nil, err
		}
	}

	if info.Config[logStreamKey] != "" {
		logStreamName = info.Config[logStreamKey]
	}
	logCreateStream := true
	if info.Config[logCreateStreamKey] != "" {
		logCreateStream, err = strconv.ParseBool(info.Config[logCreateStreamKey])
		if err != nil {
			return nil, err
		}
	}

	multilinePattern, err := parseMultilineOptions(info)
	if err != nil {
		return nil, err
	}

	containerStreamConfig := &logStreamConfig{
		logStreamName:      logStreamName,
		logGroupName:       logGroupName,
		logCreateGroup:     logCreateGroup,
		logCreateStream:    logCreateStream,
		logNonBlocking:     logNonBlocking,
		forceFlushInterval: forceFlushInterval,
		maxBufferedEvents:  maxBufferedEvents,
		multilinePattern:   multilinePattern,
	}

	return containerStreamConfig, nil
}

// Parses awslogs-multiline-pattern and awslogs-datetime-format options
// If awslogs-datetime-format is present, convert the format from strftime
// to regexp and return.
// If awslogs-multiline-pattern is present, compile regexp and return
func parseMultilineOptions(info logger.Info) (*regexp.Regexp, error) {
	dateTimeFormat := info.Config[datetimeFormatKey]
	multilinePatternKey := info.Config[multilinePatternKey]
	// strftime input is parsed into a regular expression
	if dateTimeFormat != "" {
		// %. matches each strftime format sequence and ReplaceAllStringFunc
		// looks up each format sequence in the conversion table strftimeToRegex
		// to replace with a defined regular expression
		r := regexp.MustCompile("%.")
		multilinePatternKey = r.ReplaceAllStringFunc(dateTimeFormat, func(s string) string {
			return strftimeToRegex[s]
		})
	}
	if multilinePatternKey != "" {
		multilinePattern, err := regexp.Compile(multilinePatternKey)
		if err != nil {
			return nil, errors.Wrapf(err, "awslogs could not parse multiline pattern key %q", multilinePatternKey)
		}
		return multilinePattern, nil
	}
	return nil, nil
}

// Maps strftime format strings to regex
var strftimeToRegex = map[string]string{
	/*weekdayShort          */ `%a`: `(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun)`,
	/*weekdayFull           */ `%A`: `(?:Monday|Tuesday|Wednesday|Thursday|Friday|Saturday|Sunday)`,
	/*weekdayZeroIndex      */ `%w`: `[0-6]`,
	/*dayZeroPadded         */ `%d`: `(?:0[1-9]|[1,2][0-9]|3[0,1])`,
	/*monthShort            */ `%b`: `(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)`,
	/*monthFull             */ `%B`: `(?:January|February|March|April|May|June|July|August|September|October|November|December)`,
	/*monthZeroPadded       */ `%m`: `(?:0[1-9]|1[0-2])`,
	/*yearCentury           */ `%Y`: `\d{4}`,
	/*yearZeroPadded        */ `%y`: `\d{2}`,
	/*hour24ZeroPadded      */ `%H`: `(?:[0,1][0-9]|2[0-3])`,
	/*hour12ZeroPadded      */ `%I`: `(?:0[0-9]|1[0-2])`,
	/*AM or PM              */ `%p`: "[A,P]M",
	/*minuteZeroPadded      */ `%M`: `[0-5][0-9]`,
	/*secondZeroPadded      */ `%S`: `[0-5][0-9]`,
	/*microsecondZeroPadded */ `%f`: `\d{6}`,
	/*utcOffset             */ `%z`: `[+-]\d{4}`,
	/*tzName                */ `%Z`: `[A-Z]{1,4}T`,
	/*dayOfYearZeroPadded   */ `%j`: `(?:0[0-9][1-9]|[1,2][0-9][0-9]|3[0-5][0-9]|36[0-6])`,
	/*milliseconds          */ `%L`: `\.\d{3}`,
}

// newRegionFinder is a variable such that the implementation
// can be swapped out for unit tests.
var newRegionFinder = func() (regionFinder, error) {
	s, err := session.NewSession()
	if err != nil {
		return nil, err
	}
	return ec2metadata.New(s), nil
}

// newSDKEndpoint is a variable such that the implementation
// can be swapped out for unit tests.
var newSDKEndpoint = credentialsEndpoint

// newAWSLogsClient creates the service client for Amazon CloudWatch Logs.
// Customizations to the default client from the SDK include a Docker-specific
// User-Agent string and automatic region detection using the EC2 Instance
// Metadata Service when region is otherwise unspecified.
func newAWSLogsClient(info logger.Info) (api, error) {
	var region, endpoint *string
	if os.Getenv(regionEnvKey) != "" {
		region = aws.String(os.Getenv(regionEnvKey))
	}
	if info.Config[regionKey] != "" {
		region = aws.String(info.Config[regionKey])
	}
	if info.Config[endpointKey] != "" {
		endpoint = aws.String(info.Config[endpointKey])
	}
	if region == nil || *region == "" {
		logrus.Info("Trying to get region from EC2 Metadata")
		ec2MetadataClient, err := newRegionFinder()
		if err != nil {
			logrus.WithError(err).Error("could not create EC2 metadata client")
			return nil, errors.Wrap(err, "could not create EC2 metadata client")
		}

		r, err := ec2MetadataClient.Region()
		if err != nil {
			logrus.WithError(err).Error("Could not get region from EC2 metadata, environment, or log option")
			return nil, errors.New("Cannot determine region for awslogs driver")
		}
		region = &r
	}

	sess, err := session.NewSession()
	if err != nil {
		return nil, errors.New("Failed to create a service client session for awslogs driver")
	}

	// attach region to cloudwatchlogs config
	sess.Config.Region = region

	// attach endpoint to cloudwatchlogs config
	if endpoint != nil {
		sess.Config.Endpoint = endpoint
	}

	if uri, ok := info.Config[credentialsEndpointKey]; ok {
		logrus.Debugf("Trying to get credentials from awslogs-credentials-endpoint")

		endpoint := fmt.Sprintf("%s%s", newSDKEndpoint, uri)
		creds := endpointcreds.NewCredentialsClient(*sess.Config, sess.Handlers, endpoint,
			func(p *endpointcreds.Provider) {
				p.ExpiryWindow = 5 * time.Minute
			})

		// attach credentials to cloudwatchlogs config
		sess.Config.Credentials = creds
	}

	logrus.WithFields(logrus.Fields{
		"region": *region,
	}).Debug("Created awslogs client")

	client := cloudwatchlogs.New(sess)

	client.Handlers.Build.PushBackNamed(request.NamedHandler{
		Name: "DockerUserAgentHandler",
		Fn: func(r *request.Request) {
			currentAgent := r.HTTPRequest.Header.Get(userAgentHeader)
			r.HTTPRequest.Header.Set(userAgentHeader,
				fmt.Sprintf("Docker %s (%s) %s",
					dockerversion.Version, runtime.GOOS, currentAgent))
		},
	})
	return client, nil
}

// Name returns the name of the awslogs logging driver
func (l *logStream) Name() string {
	return name
}

// BufSize returns the maximum bytes CloudWatch can handle.
func (l *logStream) BufSize() int {
	return maximumBytesPerEvent
}

// Log submits messages for logging by an instance of the awslogs logging driver
func (l *logStream) Log(msg *logger.Message) error {
	l.lock.RLock()
	defer l.lock.RUnlock()
	if l.closed {
		return errors.New("awslogs is closed")
	}
	if l.logNonBlocking {
		select {
		case l.messages <- msg:
			return nil
		default:
			return errors.New("awslogs buffer is full")
		}
	}
	l.messages <- msg
	return nil
}

// Close closes the instance of the awslogs logging driver
func (l *logStream) Close() error {
	l.lock.Lock()
	defer l.lock.Unlock()
	if !l.closed {
		close(l.messages)
	}
	l.closed = true
	return nil
}

// create creates log group and log stream for the instance of the awslogs logging driver
func (l *logStream) create() error {
	err := l.createLogStream()
	if err == nil {
		return nil
	}
	if awsErr, ok := err.(awserr.Error); ok && awsErr.Code() == resourceNotFoundCode && l.logCreateGroup {
		if err := l.createLogGroup(); err != nil {
			return errors.Wrap(err, "failed to create Cloudwatch log group")
		}
		err = l.createLogStream()
		if err == nil {
			return nil
		}
	}
	return errors.Wrap(err, "failed to create Cloudwatch log stream")
}

// createLogGroup creates a log group for the instance of the awslogs logging driver
func (l *logStream) createLogGroup() error {
	if _, err := l.client.CreateLogGroup(&cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: aws.String(l.logGroupName),
	}); err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			fields := logrus.Fields{
				"errorCode":      awsErr.Code(),
				"message":        awsErr.Message(),
				"origError":      awsErr.OrigErr(),
				"logGroupName":   l.logGroupName,
				"logCreateGroup": l.logCreateGroup,
			}
			if awsErr.Code() == resourceAlreadyExistsCode {
				// Allow creation to succeed
				logrus.WithFields(fields).Info("Log group already exists")
				return nil
			}
			logrus.WithFields(fields).Error("Failed to create log group")
		}
		return err
	}
	return nil
}

// createLogStream creates a log stream for the instance of the awslogs logging driver
func (l *logStream) createLogStream() error {
	// Directly return if we do not want to create log stream.
	if !l.logCreateStream {
		logrus.WithFields(logrus.Fields{
			"logGroupName":    l.logGroupName,
			"logStreamName":   l.logStreamName,
			"logCreateStream": l.logCreateStream,
		}).Info("Skipping creating log stream")
		return nil
	}

	input := &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  aws.String(l.logGroupName),
		LogStreamName: aws.String(l.logStreamName),
	}

	_, err := l.client.CreateLogStream(input)

	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			fields := logrus.Fields{
				"errorCode":     awsErr.Code(),
				"message":       awsErr.Message(),
				"origError":     awsErr.OrigErr(),
				"logGroupName":  l.logGroupName,
				"logStreamName": l.logStreamName,
			}
			if awsErr.Code() == resourceAlreadyExistsCode {
				// Allow creation to succeed
				logrus.WithFields(fields).Info("Log stream already exists")
				return nil
			}
			logrus.WithFields(fields).Error("Failed to create log stream")
		}
	}
	return err
}

// newTicker is used for time-based batching.  newTicker is a variable such
// that the implementation can be swapped out for unit tests.
var newTicker = func(freq time.Duration) *time.Ticker {
	return time.NewTicker(freq)
}

// collectBatch executes as a goroutine to perform batching of log events for
// submission to the log stream.  If the awslogs-multiline-pattern or
// awslogs-datetime-format options have been configured, multiline processing
// is enabled, where log messages are stored in an event buffer until a multiline
// pattern match is found, at which point the messages in the event buffer are
// pushed to CloudWatch logs as a single log event.  Multiline messages are processed
// according to the maximumBytesPerPut constraint, and the implementation only
// allows for messages to be buffered for a maximum of 2*batchPublishFrequency
// seconds.  When events are ready to be processed for submission to CloudWatch
// Logs, the processEvents method is called.  If a multiline pattern is not
// configured, log events are submitted to the processEvents method immediately.
func (l *logStream) collectBatch(created chan bool) {
	// Wait for the logstream/group to be created
	<-created
	flushInterval := l.forceFlushInterval
	if flushInterval <= 0 {
		flushInterval = defaultForceFlushInterval
	}
	ticker := newTicker(flushInterval)
	var eventBuffer []byte
	var eventBufferTimestamp int64
	var batch = newEventBatch()
	for {
		select {
		case t := <-ticker.C:
			// If event buffer is older than batch publish frequency flush the event buffer
			if eventBufferTimestamp > 0 && len(eventBuffer) > 0 {
				eventBufferAge := t.UnixNano()/int64(time.Millisecond) - eventBufferTimestamp
				eventBufferExpired := eventBufferAge >= int64(flushInterval)/int64(time.Millisecond)
				eventBufferNegative := eventBufferAge < 0
				if eventBufferExpired || eventBufferNegative {
					l.processEvent(batch, eventBuffer, eventBufferTimestamp)
					eventBuffer = eventBuffer[:0]
				}
			}
			l.publishBatch(batch)
			batch.reset()
		case msg, more := <-l.messages:
			if !more {
				// Flush event buffer and release resources
				l.processEvent(batch, eventBuffer, eventBufferTimestamp)
				l.publishBatch(batch)
				batch.reset()
				return
			}
			if eventBufferTimestamp == 0 {
				eventBufferTimestamp = msg.Timestamp.UnixNano() / int64(time.Millisecond)
			}
			line := msg.Line
			if l.multilinePattern != nil {
				lineEffectiveLen := effectiveLen(string(line))
				if l.multilinePattern.Match(line) || effectiveLen(string(eventBuffer))+lineEffectiveLen > maximumBytesPerEvent {
					// This is a new log event or we will exceed max bytes per event
					// so flush the current eventBuffer to events and reset timestamp
					l.processEvent(batch, eventBuffer, eventBufferTimestamp)
					eventBufferTimestamp = msg.Timestamp.UnixNano() / int64(time.Millisecond)
					eventBuffer = eventBuffer[:0]
				}
				// Append newline if event is less than max event size
				if lineEffectiveLen < maximumBytesPerEvent {
					line = append(line, "\n"...)
				}
				eventBuffer = append(eventBuffer, line...)
				logger.PutMessage(msg)
			} else {
				l.processEvent(batch, line, msg.Timestamp.UnixNano()/int64(time.Millisecond))
				logger.PutMessage(msg)
			}
		}
	}
}

// processEvent processes log events that are ready for submission to CloudWatch
// logs.  Batching is performed on time- and size-bases.  Time-based batching
// occurs at a 5 second interval (defined in the batchPublishFrequency const).
// Size-based batching is performed on the maximum number of events per batch
// (defined in maximumLogEventsPerPut) and the maximum number of total bytes in a
// batch (defined in maximumBytesPerPut).  Log messages are split by the maximum
// bytes per event (defined in maximumBytesPerEvent).  There is a fixed per-event
// byte overhead (defined in perEventBytes) which is accounted for in split- and
// batch-calculations.  Because the events are interpreted as UTF-8 encoded
// Unicode, invalid UTF-8 byte sequences are replaced with the Unicode
// replacement character (U+FFFD), which is a 3-byte sequence in UTF-8.  To
// compensate for that and to avoid splitting valid UTF-8 characters into
// invalid byte sequences, we calculate the length of each event assuming that
// this replacement happens.
func (l *logStream) processEvent(batch *eventBatch, bytes []byte, timestamp int64) {
	for len(bytes) > 0 {
		// Split line length so it does not exceed the maximum
		splitOffset, lineBytes := findValidSplit(string(bytes), maximumBytesPerEvent)
		line := bytes[:splitOffset]
		event := wrappedEvent{
			inputLogEvent: &cloudwatchlogs.InputLogEvent{
				Message:   aws.String(string(line)),
				Timestamp: aws.Int64(timestamp),
			},
			insertOrder: batch.count(),
		}

		added := batch.add(event, lineBytes)
		if added {
			bytes = bytes[splitOffset:]
		} else {
			l.publishBatch(batch)
			batch.reset()
		}
	}
}

// effectiveLen counts the effective number of bytes in the string, after
// UTF-8 normalization.  UTF-8 normalization includes replacing bytes that do
// not constitute valid UTF-8 encoded Unicode codepoints with the Unicode
// replacement codepoint U+FFFD (a 3-byte UTF-8 sequence, represented in Go as
// utf8.RuneError)
func effectiveLen(line string) int {
	effectiveBytes := 0
	for _, rune := range line {
		effectiveBytes += utf8.RuneLen(rune)
	}
	return effectiveBytes
}

// findValidSplit finds the byte offset to split a string without breaking valid
// Unicode codepoints given a maximum number of total bytes.  findValidSplit
// returns the byte offset for splitting a string or []byte, as well as the
// effective number of bytes if the string were normalized to replace invalid
// UTF-8 encoded bytes with the Unicode replacement character (a 3-byte UTF-8
// sequence, represented in Go as utf8.RuneError)
func findValidSplit(line string, maxBytes int) (splitOffset, effectiveBytes int) {
	for offset, rune := range line {
		splitOffset = offset
		if effectiveBytes+utf8.RuneLen(rune) > maxBytes {
			return splitOffset, effectiveBytes
		}
		effectiveBytes += utf8.RuneLen(rune)
	}
	splitOffset = len(line)
	return
}

// publishBatch calls PutLogEvents for a given set of InputLogEvents,
// accounting for sequencing requirements (each request must reference the
// sequence token returned by the previous request).
func (l *logStream) publishBatch(batch *eventBatch) {
	if batch.isEmpty() {
		return
	}
	cwEvents := unwrapEvents(batch.events())

	nextSequenceToken, err := l.putLogEvents(cwEvents, l.sequenceToken)

	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			if awsErr.Code() == dataAlreadyAcceptedCode {
				// already submitted, just grab the correct sequence token
				parts := strings.Split(awsErr.Message(), " ")
				nextSequenceToken = &parts[len(parts)-1]
				logrus.WithFields(logrus.Fields{
					"errorCode":     awsErr.Code(),
					"message":       awsErr.Message(),
					"logGroupName":  l.logGroupName,
					"logStreamName": l.logStreamName,
				}).Info("Data already accepted, ignoring error")
				err = nil
			} else if awsErr.Code() == invalidSequenceTokenCode {
				// sequence code is bad, grab the correct one and retry
				parts := strings.Split(awsErr.Message(), " ")
				token := parts[len(parts)-1]
				nextSequenceToken, err = l.putLogEvents(cwEvents, &token)
			}
		}
	}
	if err != nil {
		logrus.Error(err)
	} else {
		l.sequenceToken = nextSequenceToken
	}
}

// putLogEvents wraps the PutLogEvents API
func (l *logStream) putLogEvents(events []*cloudwatchlogs.InputLogEvent, sequenceToken *string) (*string, error) {
	input := &cloudwatchlogs.PutLogEventsInput{
		LogEvents:     events,
		SequenceToken: sequenceToken,
		LogGroupName:  aws.String(l.logGroupName),
		LogStreamName: aws.String(l.logStreamName),
	}
	resp, err := l.client.PutLogEvents(input)
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			logrus.WithFields(logrus.Fields{
				"errorCode":     awsErr.Code(),
				"message":       awsErr.Message(),
				"origError":     awsErr.OrigErr(),
				"logGroupName":  l.logGroupName,
				"logStreamName": l.logStreamName,
			}).Error("Failed to put log events")
		}
		return nil, err
	}
	return resp.NextSequenceToken, nil
}

// ValidateLogOpt looks for awslogs-specific log options awslogs-region, awslogs-endpoint
// awslogs-group, awslogs-stream, awslogs-create-group, awslogs-datetime-format,
// awslogs-multiline-pattern
func ValidateLogOpt(cfg map[string]string) error {
	for key := range cfg {
		switch key {
		case logGroupKey:
		case logStreamKey:
		case logCreateGroupKey:
		case regionKey:
		case endpointKey:
		case tagKey:
		case datetimeFormatKey:
		case multilinePatternKey:
		case credentialsEndpointKey:
		case forceFlushIntervalKey:
		case maxBufferedEventsKey:
		default:
			return fmt.Errorf("unknown log opt '%s' for %s log driver", key, name)
		}
	}
	if cfg[logGroupKey] == "" {
		return fmt.Errorf("must specify a value for log opt '%s'", logGroupKey)
	}
	if cfg[logCreateGroupKey] != "" {
		if _, err := strconv.ParseBool(cfg[logCreateGroupKey]); err != nil {
			return fmt.Errorf("must specify valid value for log opt '%s': %v", logCreateGroupKey, err)
		}
	}
	if cfg[forceFlushIntervalKey] != "" {
		if value, err := strconv.Atoi(cfg[forceFlushIntervalKey]); err != nil || value <= 0 {
			return fmt.Errorf("must specify a positive integer for log opt '%s': %v", forceFlushIntervalKey, cfg[forceFlushIntervalKey])
		}
	}
	if cfg[maxBufferedEventsKey] != "" {
		if value, err := strconv.Atoi(cfg[maxBufferedEventsKey]); err != nil || value <= 0 {
			return fmt.Errorf("must specify a positive integer for log opt '%s': %v", maxBufferedEventsKey, cfg[maxBufferedEventsKey])
		}
	}
	_, datetimeFormatKeyExists := cfg[datetimeFormatKey]
	_, multilinePatternKeyExists := cfg[multilinePatternKey]
	if datetimeFormatKeyExists && multilinePatternKeyExists {
		return fmt.Errorf("you cannot configure log opt '%s' and '%s' at the same time", datetimeFormatKey, multilinePatternKey)
	}
	return nil
}

// Len returns the length of a byTimestamp slice.  Len is required by the
// sort.Interface interface.
func (slice byTimestamp) Len() int {
	return len(slice)
}

// Less compares two values in a byTimestamp slice by Timestamp.  Less is
// required by the sort.Interface interface.
func (slice byTimestamp) Less(i, j int) bool {
	iTimestamp, jTimestamp := int64(0), int64(0)
	if slice != nil && slice[i].inputLogEvent.Timestamp != nil {
		iTimestamp = *slice[i].inputLogEvent.Timestamp
	}
	if slice != nil && slice[j].inputLogEvent.Timestamp != nil {
		jTimestamp = *slice[j].inputLogEvent.Timestamp
	}
	if iTimestamp == jTimestamp {
		return slice[i].insertOrder < slice[j].insertOrder
	}
	return iTimestamp < jTimestamp
}

// Swap swaps two values in a byTimestamp slice with each other.  Swap is
// required by the sort.Interface interface.
func (slice byTimestamp) Swap(i, j int) {
	slice[i], slice[j] = slice[j], slice[i]
}

func unwrapEvents(events []wrappedEvent) []*cloudwatchlogs.InputLogEvent {
	cwEvents := make([]*cloudwatchlogs.InputLogEvent, len(events))
	for i, input := range events {
		cwEvents[i] = input.inputLogEvent
	}
	return cwEvents
}

func newEventBatch() *eventBatch {
	return &eventBatch{
		batch: make([]wrappedEvent, 0),
		bytes: 0,
	}
}

// events returns a slice of wrappedEvents sorted in order of their
// timestamps and then by their insertion order (see `byTimestamp`).
//
// Warning: this method is not threadsafe and must not be used
// concurrently.
func (b *eventBatch) events() []wrappedEvent {
	sort.Sort(byTimestamp(b.batch))
	return b.batch
}

// add adds an event to the batch of events accounting for the
// necessary overhead for an event to be logged. An error will be
// returned if the event cannot be added to the batch due to service
// limits.
//
// Warning: this method is not threadsafe and must not be used
// concurrently.
func (b *eventBatch) add(event wrappedEvent, size int) bool {
	addBytes := size + perEventBytes

	// verify we are still within service limits
	switch {
	case len(b.batch)+1 > maximumLogEventsPerPut:
		return false
	case b.bytes+addBytes > maximumBytesPerPut:
		return false
	}

	b.bytes += addBytes
	b.batch = append(b.batch, event)

	return true
}

// count is the number of batched events.  Warning: this method
// is not threadsafe and must not be used concurrently.
func (b *eventBatch) count() int {
	return len(b.batch)
}

// size is the total number of bytes that the batch represents.
//
// Warning: this method is not threadsafe and must not be used
// concurrently.
func (b *eventBatch) size() int {
	return b.bytes
}

func (b *eventBatch) isEmpty() bool {
	zeroEvents := b.count() == 0
	zeroSize := b.size() == 0
	return zeroEvents && zeroSize
}

// reset prepares the batch for reuse.
func (b *eventBatch) reset() {
	b.bytes = 0
	b.batch = b.batch[:0]
}
