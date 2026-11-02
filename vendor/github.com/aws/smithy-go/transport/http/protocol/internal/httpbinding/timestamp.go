package httpbinding

import (
	"fmt"
	"strconv"
	"time"

	"github.com/aws/smithy-go"
	smithytime "github.com/aws/smithy-go/time"
	"github.com/aws/smithy-go/traits"
)

func timestampFormat(schema *smithy.Schema, fallback string) string {
	if tf, ok := smithy.SchemaTrait[*traits.TimestampFormat](schema); ok {
		return tf.Format
	}
	return fallback
}

func formatTimestamp(schema *smithy.Schema, fallback string, v time.Time) string {
	switch timestampFormat(schema, fallback) {
	case "http-date":
		return smithytime.FormatHTTPDate(v)
	case "date-time":
		return smithytime.FormatDateTime(v)
	default: // "epoch-seconds"
		return strconv.FormatFloat(smithytime.FormatEpochSeconds(v), 'f', -1, 64)
	}
}

func parseTimestamp(schema *smithy.Schema, fallback, s string) (time.Time, error) {
	switch timestampFormat(schema, fallback) {
	case "http-date":
		return smithytime.ParseHTTPDate(s)
	case "date-time":
		return smithytime.ParseDateTime(s)
	default: // "epoch-seconds"
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse epoch-seconds %q: %w", s, err)
		}
		return smithytime.ParseEpochSeconds(v), nil
	}
}
