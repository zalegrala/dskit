package ruler

import (
	"context"

	"github.com/stretchr/testify/mock"

	"github.com/grafana/dskit/dskitpb"
)

type pusherMock struct {
	mock.Mock
}

func newPusherMock() *pusherMock {
	return &pusherMock{}
}

func (m *pusherMock) Push(ctx context.Context, req *dskitpb.WriteRequest) (*dskitpb.WriteResponse, error) {
	args := m.Called(ctx, req)
	return args.Get(0).(*dskitpb.WriteResponse), args.Error(1)
}

func (m *pusherMock) MockPush(res *dskitpb.WriteResponse, err error) {
	m.On("Push", mock.Anything, mock.Anything).Return(res, err)
}
