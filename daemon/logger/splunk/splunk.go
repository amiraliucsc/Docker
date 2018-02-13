// Package splunk provides the log driver for forwarding server logs to
// Splunk HTTP Event Collector endpoint.
package splunk // import "github.com/docker/docker/daemon/logger/splunk"

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/loggerutils"
	"github.com/docker/docker/pkg/pools"
	"github.com/docker/docker/pkg/urlutil"
	"github.com/sirupsen/logrus"
)

const (
	driverName                    = "splunk"
	splunkURLKey                  = "splunk-url"
	splunkTokenKey                = "splunk-token"
	splunkSourceKey               = "splunk-source"
	splunkSourceTypeKey           = "splunk-sourcetype"
	splunkIndexKey                = "splunk-index"
	splunkCAPathKey               = "splunk-capath"
	splunkCANameKey               = "splunk-caname"
	splunkInsecureSkipVerifyKey   = "splunk-insecureskipverify"
	splunkFormatKey               = "splunk-format"
	splunkVerifyConnectionKey     = "splunk-verify-connection"
	splunkGzipCompressionKey      = "splunk-gzip"
	splunkGzipCompressionLevelKey = "splunk-gzip-level"
	envKey                        = "env"
	envRegexKey                   = "env-regex"
	labelsKey                     = "labels"
	tagKey                        = "tag"
)

const (
	// How often do we send messages (if we are not reaching batch size)
	defaultPostMessagesFrequency = 5 * time.Second
	// How big can be batch of messages
	defaultPostMessagesBatchSize = 1000
	// Maximum number of messages we can store in buffer
	defaultBufferMaximum = 10 * defaultPostMessagesBatchSize
	// Number of messages allowed to be queued in the channel
	defaultStreamChannelSize = 4 * defaultPostMessagesBatchSize
	// maxResponseSize is the max amount that will be read from an http response
	maxResponseSize = 1024
)

const (
	envVarPostMessagesFrequency = "SPLUNK_LOGGING_DRIVER_POST_MESSAGES_FREQUENCY"
	envVarPostMessagesBatchSize = "SPLUNK_LOGGING_DRIVER_POST_MESSAGES_BATCH_SIZE"
	envVarBufferMaximum         = "SPLUNK_LOGGING_DRIVER_BUFFER_MAX"
	envVarStreamChannelSize     = "SPLUNK_LOGGING_DRIVER_CHANNEL_SIZE"
)

var batchSendTimeout = 30 * time.Second

type splunkLoggerInterface interface {
	logger.Logger
	worker()
}

type splunkLogger struct {
	client    *http.Client
	transport *http.Transport

	url         string
	auth        string
	nullMessage *splunkMessage

	// http compression
	gzipCompression      bool
	gzipCompressionLevel int

	// Advanced options
	postMessagesFrequency time.Duration
	postMessagesBatchSize int
	bufferMaximum         int

	// For synchronization between background worker and logger.
	// We use channel to send messages to worker go routine.
	// All other variables for blocking Close call before we flush all messages to HEC
	stream     chan *splunkMessage
	lock       sync.RWMutex
	closed     bool
	closedCond *sync.Cond
}

type splunkLoggerInline struct {
	*splunkLogger

	nullEvent *splunkMessageEvent
}

type splunkLoggerJSON struct {
	*splunkLoggerInline
}

type splunkLoggerRaw struct {
	*splunkLogger

	prefix []byte
}

type splunkMessage struct {
	Event      interface{} `json:"event"`
	Time       string      `json:"time"`
	Host       string      `json:"host"`
	Source     string      `json:"source,omitempty"`
	SourceType string      `json:"sourcetype,omitempty"`
	Index      string      `json:"index,omitempty"`
}

type splunkMessageEvent struct {
	Line   interface{}       `json:"line"`
	Source string            `json:"source"`
	Tag    string            `json:"tag,omitempty"`
	Attrs  map[string]string `json:"attrs,omitempty"`
}

const (
	splunkFormatRaw    = "raw"
	splunkFormatJSON   = "json"
	splunkFormatInline = "inline"
)

func init() {
	if err := logger.RegisterLogDriver(driverName, New); err != nil {
		logrus.Fatal(err)
	}
	if err := logger.RegisterLogOptValidator(driverName, ValidateLogOpt); err != nil {
		logrus.Fatal(err)
	}
}

// New creates splunk logger driver using configuration passed in context
func New(info logger.Info) (logger.Logger, error) {
	hostname, err := info.Hostname()
	if err != nil {
		return nil, fmt.Errorf("%s: cannot access hostname to set source field", driverName)
	}

	// Parse and validate Splunk URL
	splunkURL, err := parseURL(info)
	if err != nil {
		return nil, err
	}

	// Splunk Token is required parameter
	splunkToken, ok := info.Config[splunkTokenKey]
	if !ok {
		return nil, fmt.Errorf("%s: %s is expected", driverName, splunkTokenKey)
	}

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	// Splunk is using autogenerated certificates by default,
	// allow users to trust them with skipping verification
	if insecureSkipVerifyStr, ok := info.Config[splunkInsecureSkipVerifyKey]; ok {
		insecureSkipVerify, err := strconv.ParseBool(insecureSkipVerifyStr)
		if err != nil {
			return nil, err
		}
		tlsConfig.InsecureSkipVerify = insecureSkipVerify
	}

	// If path to the root certificate is provided - load it
	if caPath, ok := info.Config[splunkCAPathKey]; ok {
		caCert, err := ioutil.ReadFile(caPath)
		if err != nil {
			return nil, err
		}
		caPool := x509.NewCertPool()
		caPool.AppendCertsFromPEM(caCert)
		tlsConfig.RootCAs = caPool
	}

	if caName, ok := info.Config[splunkCANameKey]; ok {
		tlsConfig.ServerName = caName
	}

	gzipCompression := false
	if gzipCompressionStr, ok := info.Config[splunkGzipCompressionKey]; ok {
		gzipCompression, err = strconv.ParseBool(gzipCompressionStr)
		if err != nil {
			return nil, err
		}
	}

	gzipCompressionLevel := gzip.DefaultCompression
	if gzipCompressionLevelStr, ok := info.Config[splunkGzipCompressionLevelKey]; ok {
		var err error
		gzipCompressionLevel64, err := strconv.ParseInt(gzipCompressionLevelStr, 10, 32)
		if err != nil {
			return nil, err
		}
		gzipCompressionLevel = int(gzipCompressionLevel64)
		if gzipCompressionLevel < gzip.DefaultCompression || gzipCompressionLevel > gzip.BestCompression {
			err := fmt.Errorf("not supported level '%s' for %s (supported values between %d and %d)",
				gzipCompressionLevelStr, splunkGzipCompressionLevelKey, gzip.DefaultCompression, gzip.BestCompression)
			return nil, err
		}
	}

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
		Proxy:           http.ProxyFromEnvironment,
	}
	client := &http.Client{
		Transport: transport,
	}

	source := info.Config[splunkSourceKey]
	sourceType := info.Config[splunkSourceTypeKey]
	index := info.Config[splunkIndexKey]

	var nullMessage = &splunkMessage{
		Host:       hostname,
		Source:     source,
		SourceType: sourceType,
		Index:      index,
	}

	// Allow user to remove tag from the messages by setting tag to empty string
	tag := ""
	if tagTemplate, ok := info.Config[tagKey]; !ok || tagTemplate != "" {
		tag, err = loggerutils.ParseLogTag(info, loggerutils.DefaultTemplate)
		if err != nil {
			return nil, err
		}
	}

	attrs, err := info.ExtraAttributes(nil)
	if err != nil {
		return nil, err
	}

	var (
		postMessagesFrequency = getAdvancedOptionDuration(envVarPostMessagesFrequency, defaultPostMessagesFrequency)
		postMessagesBatchSize = getAdvancedOptionInt(envVarPostMessagesBatchSize, defaultPostMessagesBatchSize)
		bufferMaximum         = getAdvancedOptionInt(envVarBufferMaximum, defaultBufferMaximum)
		streamChannelSize     = getAdvancedOptionInt(envVarStreamChannelSize, defaultStreamChannelSize)
	)

	logger := &splunkLogger{
		client:                client,
		transport:             transport,
		url:                   splunkURL.String(),
		auth:                  "Splunk " + splunkToken,
		nullMessage:           nullMessage,
		gzipCompression:       gzipCompression,
		gzipCompressionLevel:  gzipCompressionLevel,
		stream:                make(chan *splunkMessage, streamChannelSize),
		postMessagesFrequency: postMessagesFrequency,
		postMessagesBatchSize: postMessagesBatchSize,
		bufferMaximum:         bufferMaximum,
	}

	// By default we verify connection, but we allow use to skip that
	verifyConnection := true
	if verifyConnectionStr, ok := info.Config[splunkVerifyConnectionKey]; ok {
		var err error
		verifyConnection, err = strconv.ParseBool(verifyConnectionStr)
		if err != nil {
			return nil, err
		}
	}
	if verifyConnection {
		err = verifySplunkConnection(logger)
		if err != nil {
			return nil, err
		}
	}

	var splunkFormat string
	if splunkFormatParsed, ok := info.Config[splunkFormatKey]; ok {
		switch splunkFormatParsed {
		case splunkFormatInline:
		case splunkFormatJSON:
		case splunkFormatRaw:
		default:
			return nil, fmt.Errorf("Unknown format specified %s, supported formats are inline, json and raw", splunkFormat)
		}
		splunkFormat = splunkFormatParsed
	} else {
		splunkFormat = splunkFormatInline
	}

	var loggerWrapper splunkLoggerInterface

	switch splunkFormat {
	case splunkFormatInline:
		nullEvent := &splunkMessageEvent{
			Tag:   tag,
			Attrs: attrs,
		}

		loggerWrapper = &splunkLoggerInline{logger, nullEvent}
	case splunkFormatJSON:
		nullEvent := &splunkMessageEvent{
			Tag:   tag,
			Attrs: attrs,
		}

		loggerWrapper = &splunkLoggerJSON{&splunkLoggerInline{logger, nullEvent}}
	case splunkFormatRaw:
		var prefix bytes.Buffer
		if tag != "" {
			prefix.WriteString(tag)
			prefix.WriteString(" ")
		}
		for key, value := range attrs {
			prefix.WriteString(key)
			prefix.WriteString("=")
			prefix.WriteString(value)
			prefix.WriteString(" ")
		}

		loggerWrapper = &splunkLoggerRaw{logger, prefix.Bytes()}
	default:
		return nil, fmt.Errorf("Unexpected format %s", splunkFormat)
	}

	go loggerWrapper.worker()

	return loggerWrapper, nil
}

func (l *splunkLoggerInline) Log(msg *logger.Message) error {
	message := l.createSplunkMessage(msg)

	event := *l.nullEvent
	event.Line = string(msg.Line)
	event.Source = msg.Source

	message.Event = &event
	logger.PutMessage(msg)
	return l.queueMessageAsync(message)
}

func (l *splunkLoggerJSON) Log(msg *logger.Message) error {
	message := l.createSplunkMessage(msg)
	event := *l.nullEvent

	var rawJSONMessage json.RawMessage
	if err := json.Unmarshal(msg.Line, &rawJSONMessage); err == nil {
		event.Line = &rawJSONMessage
	} else {
		event.Line = string(msg.Line)
	}

	event.Source = msg.Source

	message.Event = &event
	logger.PutMessage(msg)
	return l.queueMessageAsync(message)
}

func (l *splunkLoggerRaw) Log(msg *logger.Message) error {
	// empty or whitespace-only messages are not accepted by HEC
	if strings.TrimSpace(string(msg.Line)) == "" {
		return nil
	}

	message := l.createSplunkMessage(msg)

	message.Event = string(append(l.prefix, msg.Line...))
	logger.PutMessage(msg)
	return l.queueMessageAsync(message)
}

func (l *splunkLogger) queueMessageAsync(message *splunkMessage) error {
	l.lock.RLock()
	defer l.lock.RUnlock()
	if l.closedCond != nil {
		return fmt.Errorf("%s: driver is closed", driverName)
	}
	l.stream <- message
	return nil
}

func (l *splunkLogger) worker() {
	timer := time.NewTicker(l.postMessagesFrequency)
	var messages []*splunkMessage
	for {
		select {
		case message, open := <-l.stream:
			if !open {
				l.postMessages(messages, true)
				l.lock.Lock()
				defer l.lock.Unlock()
				l.transport.CloseIdleConnections()
				l.closed = true
				l.closedCond.Signal()
				return
			}
			messages = append(messages, message)
			// Only sending when we get exactly to the batch size,
			// This also helps not to fire postMessages on every new message,
			// when previous try failed.
			if len(messages)%l.postMessagesBatchSize == 0 {
				messages = l.postMessages(messages, false)
			}
		case <-timer.C:
			messages = l.postMessages(messages, false)
		}
	}
}

func (l *splunkLogger) postMessages(messages []*splunkMessage, lastChance bool) []*splunkMessage {
	messagesLen := len(messages)

	ctx, cancel := context.WithTimeout(context.Background(), batchSendTimeout)
	defer cancel()

	for i := 0; i < messagesLen; i += l.postMessagesBatchSize {
		upperBound := i + l.postMessagesBatchSize
		if upperBound > messagesLen {
			upperBound = messagesLen
		}

		if err := l.tryPostMessages(ctx, messages[i:upperBound]); err != nil {
			logrus.WithError(err).WithField("module", "logger/splunk").Warn("Error while sending logs")
			if messagesLen-i >= l.bufferMaximum || lastChance {
				// If this is last chance - print them all to the daemon log
				if lastChance {
					upperBound = messagesLen
				}
				// Not all sent, but buffer has got to its maximum, let's log all messages
				// we could not send and return buffer minus one batch size
				for j := i; j < upperBound; j++ {
					if jsonEvent, err := json.Marshal(messages[j]); err != nil {
						logrus.Error(err)
					} else {
						logrus.Error(fmt.Errorf("Failed to send a message '%s'", string(jsonEvent)))
					}
				}
				return messages[upperBound:messagesLen]
			}
			// Not all sent, returning buffer from where we have not sent messages
			return messages[i:messagesLen]
		}
	}
	// All sent, return empty buffer
	return messages[:0]
}

func (l *splunkLogger) tryPostMessages(ctx context.Context, messages []*splunkMessage) error {
	if len(messages) == 0 {
		return nil
	}
	var buffer bytes.Buffer
	var writer io.Writer
	var gzipWriter *gzip.Writer
	var err error
	// If gzip compression is enabled - create gzip writer with specified compression
	// level. If gzip compression is disabled, use standard buffer as a writer
	if l.gzipCompression {
		gzipWriter, err = gzip.NewWriterLevel(&buffer, l.gzipCompressionLevel)
		if err != nil {
			return err
		}
		writer = gzipWriter
	} else {
		writer = &buffer
	}
	for _, message := range messages {
		jsonEvent, err := json.Marshal(message)
		if err != nil {
			return err
		}
		if _, err := writer.Write(jsonEvent); err != nil {
			return err
		}
	}
	// If gzip compression is enabled, tell it, that we are done
	if l.gzipCompression {
		err = gzipWriter.Close()
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequest("POST", l.url, bytes.NewBuffer(buffer.Bytes()))
	if err != nil {
		return err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", l.auth)
	// Tell if we are sending gzip compressed body
	if l.gzipCompression {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		pools.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		rdr := io.LimitReader(resp.Body, maxResponseSize)
		body, err := ioutil.ReadAll(rdr)
		if err != nil {
			return err
		}
		return fmt.Errorf("%s: failed to send event - %s - %s", driverName, resp.Status, string(body))
	}
	return nil
}

func (l *splunkLogger) Close() error {
	l.lock.Lock()
	defer l.lock.Unlock()
	if l.closedCond == nil {
		l.closedCond = sync.NewCond(&l.lock)
		close(l.stream)
		for !l.closed {
			l.closedCond.Wait()
		}
	}
	return nil
}

func (l *splunkLogger) Name() string {
	return driverName
}

func (l *splunkLogger) createSplunkMessage(msg *logger.Message) *splunkMessage {
	message := *l.nullMessage
	message.Time = fmt.Sprintf("%f", float64(msg.Timestamp.UnixNano())/float64(time.Second))
	return &message
}

// ValidateLogOpt looks for all supported by splunk driver options
func ValidateLogOpt(cfg map[string]string) error {
	for key := range cfg {
		switch key {
		case splunkURLKey:
		case splunkTokenKey:
		case splunkSourceKey:
		case splunkSourceTypeKey:
		case splunkIndexKey:
		case splunkCAPathKey:
		case splunkCANameKey:
		case splunkInsecureSkipVerifyKey:
		case splunkFormatKey:
		case splunkVerifyConnectionKey:
		case splunkGzipCompressionKey:
		case splunkGzipCompressionLevelKey:
		case envKey:
		case envRegexKey:
		case labelsKey:
		case tagKey:
		default:
			return fmt.Errorf("unknown log opt '%s' for %s log driver", key, driverName)
		}
	}
	return nil
}

func parseURL(info logger.Info) (*url.URL, error) {
	splunkURLStr, ok := info.Config[splunkURLKey]
	if !ok {
		return nil, fmt.Errorf("%s: %s is expected", driverName, splunkURLKey)
	}

	splunkURL, err := url.Parse(splunkURLStr)
	if err != nil {
		return nil, fmt.Errorf("%s: failed to parse %s as url value in %s", driverName, splunkURLStr, splunkURLKey)
	}

	if !urlutil.IsURL(splunkURLStr) ||
		!splunkURL.IsAbs() ||
		(splunkURL.Path != "" && splunkURL.Path != "/") ||
		splunkURL.RawQuery != "" ||
		splunkURL.Fragment != "" {
		return nil, fmt.Errorf("%s: expected format scheme://dns_name_or_ip:port for %s", driverName, splunkURLKey)
	}

	splunkURL.Path = "/services/collector/event/1.0"

	return splunkURL, nil
}

func verifySplunkConnection(l *splunkLogger) error {
	req, err := http.NewRequest(http.MethodOptions, l.url, nil)
	if err != nil {
		return err
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		pools.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		rdr := io.LimitReader(resp.Body, maxResponseSize)
		body, err := ioutil.ReadAll(rdr)
		if err != nil {
			return err
		}
		return fmt.Errorf("%s: failed to verify connection - %s - %s", driverName, resp.Status, string(body))
	}
	return nil
}

func getAdvancedOptionDuration(envName string, defaultValue time.Duration) time.Duration {
	valueStr := os.Getenv(envName)
	if valueStr == "" {
		return defaultValue
	}
	parsedValue, err := time.ParseDuration(valueStr)
	if err != nil {
		logrus.Error(fmt.Sprintf("Failed to parse value of %s as duration. Using default %v. %v", envName, defaultValue, err))
		return defaultValue
	}
	return parsedValue
}

func getAdvancedOptionInt(envName string, defaultValue int) int {
	valueStr := os.Getenv(envName)
	if valueStr == "" {
		return defaultValue
	}
	parsedValue, err := strconv.ParseInt(valueStr, 10, 32)
	if err != nil {
		logrus.Error(fmt.Sprintf("Failed to parse value of %s as integer. Using default %d. %v", envName, defaultValue, err))
		return defaultValue
	}
	return int(parsedValue)
}
