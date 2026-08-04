package productcapability

import (
	"context"
	"time"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DescribeCapabilities 返回所有可用的 Product capabilities
func (s *Server) DescribeCapabilities(
	ctx context.Context,
	req *capv1.DescribeCapabilitiesRequest,
) (*capv1.DescribeCapabilitiesResponse, error) {
	descriptors := s.registry.List()

	// TODO: 计算 catalog digest
	return &capv1.DescribeCapabilitiesResponse{
		Capabilities:   descriptors,
		CatalogVersion: 1,
		CatalogDigest:  []byte("TODO"),
	}, nil
}

// Query 执行无副作用查询
func (s *Server) Query(
	ctx context.Context,
	req *capv1.QueryRequest,
) (*capv1.QueryResponse, error) {
	// 获取 read semaphore
	if err := s.acquireReadSem(ctx); err != nil {
		return nil, err
	}
	defer s.releaseReadSem()

	// 查找 capability
	provider, ok := s.registry.Get(req.CapabilityId)
	if !ok {
		return &capv1.QueryResponse{
			Error: &capv1.CapabilityError{
				Code:    capv1.ErrorCode_ERROR_CODE_NOT_FOUND,
				Message: "capability not found",
			},
		}, nil
	}

	// 验证 readiness
	if !provider.Descriptor.Readiness {
		return &capv1.QueryResponse{
			Error: &capv1.CapabilityError{
				Code:    capv1.ErrorCode_ERROR_CODE_NOT_READY,
				Message: provider.Descriptor.ReadinessReason,
			},
		}, nil
	}

	// TODO: 验证 permission/scope
	// TODO: 验证 audience

	// 调用 handler
	result, err := provider.Handler(ctx, req.RequestJson)
	if err != nil {
		return &capv1.QueryResponse{
			Error: &capv1.CapabilityError{
				Code:    capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED,
				Message: err.Error(),
			},
		}, nil
	}

	return &capv1.QueryResponse{
		ResultJson: result,
	}, nil
}

// StartOperation 启动一个 operation
func (s *Server) StartOperation(
	ctx context.Context,
	req *capv1.StartOperationRequest,
) (*capv1.StartOperationResponse, error) {
	// 获取 mutation semaphore
	if err := s.acquireMutationSem(ctx); err != nil {
		return nil, err
	}
	defer s.releaseMutationSem()

	// TODO: 实现 operation 启动逻辑
	return nil, status.Error(codes.Unimplemented, "StartOperation not implemented")
}

// GetOperation 获取 operation 状态
func (s *Server) GetOperation(
	ctx context.Context,
	req *capv1.GetOperationRequest,
) (*capv1.GetOperationResponse, error) {
	// TODO: 实现 get operation 逻辑
	return nil, status.Error(codes.Unimplemented, "GetOperation not implemented")
}

// WatchOperation 监听 operation 事件流
func (s *Server) WatchOperation(
	req *capv1.WatchOperationRequest,
	stream capv1.ProductCapabilityService_WatchOperationServer,
) error {
	// TODO: 实现 watch operation 逻辑
	return status.Error(codes.Unimplemented, "WatchOperation not implemented")
}

// CancelOperation 取消 operation
func (s *Server) CancelOperation(
	ctx context.Context,
	req *capv1.CancelOperationRequest,
) (*capv1.CancelOperationResponse, error) {
	// TODO: 实现 cancel operation 逻辑
	return nil, status.Error(codes.Unimplemented, "CancelOperation not implemented")
}

// ReconcileOperation 协调不确定状态的 operation
func (s *Server) ReconcileOperation(
	ctx context.Context,
	req *capv1.ReconcileOperationRequest,
) (*capv1.ReconcileOperationResponse, error) {
	// TODO: 实现 reconcile operation 逻辑
	return nil, status.Error(codes.Unimplemented, "ReconcileOperation not implemented")
}

// acquireReadSem 获取 read semaphore
func (s *Server) acquireReadSem(ctx context.Context) error {
	select {
	case s.readSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return status.Error(codes.DeadlineExceeded, "timeout acquiring read semaphore")
	case <-time.After(1 * time.Second):
		return status.Error(codes.ResourceExhausted, "read semaphore exhausted")
	}
}

// releaseReadSem 释放 read semaphore
func (s *Server) releaseReadSem() {
	<-s.readSem
}

// acquireMutationSem 获取 mutation semaphore
func (s *Server) acquireMutationSem(ctx context.Context) error {
	select {
	case s.mutationSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return status.Error(codes.DeadlineExceeded, "timeout acquiring mutation semaphore")
	case <-time.After(1 * time.Second):
		return status.Error(codes.ResourceExhausted, "mutation semaphore exhausted")
	}
}

// releaseMutationSem 释放 mutation semaphore
func (s *Server) releaseMutationSem() {
	<-s.mutationSem
}
