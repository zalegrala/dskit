package grpcutil

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	"github.com/grafana/dskit/dskitpb"
	"github.com/stretchr/testify/assert"
)

func TestParseProtoReader(t *testing.T) {
	// 47 bytes compressed and 53 uncompressed
	req := &dskitpb.PreallocWriteRequest{
		WriteRequest: dskitpb.WriteRequest{
			Timeseries: []dskitpb.PreallocTimeseries{
				{
					TimeSeries: &dskitpb.TimeSeries{
						Labels: []dskitpb.LabelAdapter{
							{Name: "foo", Value: "bar"},
						},
						Samples: []dskitpb.Sample{
							{Value: 10, TimestampMs: 1},
							{Value: 20, TimestampMs: 2},
							{Value: 30, TimestampMs: 3},
						},
						Exemplars: []dskitpb.Exemplar{},
					},
				},
			},
		},
	}

	for _, tt := range []struct {
		name           string
		compression    CompressionType
		maxSize        int
		expectErr      bool
		useBytesBuffer bool
	}{
		{"rawSnappy", RawSnappy, 53, false, false},
		{"noCompression", NoCompression, 53, false, false},
		{"too big rawSnappy", RawSnappy, 10, true, false},
		{"too big decoded rawSnappy", RawSnappy, 50, true, false},
		{"too big noCompression", NoCompression, 10, true, false},

		{"bytesbuffer rawSnappy", RawSnappy, 53, false, true},
		{"bytesbuffer noCompression", NoCompression, 53, false, true},
		{"bytesbuffer too big rawSnappy", RawSnappy, 10, true, true},
		{"bytesbuffer too big decoded rawSnappy", RawSnappy, 50, true, true},
		{"bytesbuffer too big noCompression", NoCompression, 10, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			assert.Nil(t, SerializeProtoResponse(w, req, tt.compression))
			var fromWire dskitpb.PreallocWriteRequest

			reader := w.Result().Body
			if tt.useBytesBuffer {
				buf := bytes.Buffer{}
				_, err := buf.ReadFrom(reader)
				assert.Nil(t, err)
				reader = bytesBuffered{Buffer: &buf}
			}

			err := ParseProtoReader(context.Background(), reader, 0, tt.maxSize, &fromWire, tt.compression)
			if tt.expectErr {
				assert.NotNil(t, err)
				return
			}
			assert.Nil(t, err)
			assert.Equal(t, req, &fromWire)
		})
	}
}

type bytesBuffered struct {
	*bytes.Buffer
}

func (b bytesBuffered) Close() error {
	return nil
}

func (b bytesBuffered) BytesBuffer() *bytes.Buffer {
	return b.Buffer
}
