package handlers

import (
	"context"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestModelExecutionUsageAliasPreservesPhysicalModel(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(*BaseAPIHandler, ModelExecutionRequest) *interfaces.ErrorMessage
	}{
		{
			name: "execute",
			invoke: func(handler *BaseAPIHandler, req ModelExecutionRequest) *interfaces.ErrorMessage {
				_, errMsg := handler.ExecuteModel(context.Background(), req)
				return errMsg
			},
		},
		{
			name: "count",
			invoke: func(handler *BaseAPIHandler, req ModelExecutionRequest) *interfaces.ErrorMessage {
				_, errMsg := handler.CountModelTokens(context.Background(), req)
				return errMsg
			},
		},
		{
			name: "stream",
			invoke: func(handler *BaseAPIHandler, req ModelExecutionRequest) *interfaces.ErrorMessage {
				req.Stream = true
				_, errMsg := handler.ExecuteModelStream(context.Background(), req)
				return errMsg
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			physicalModel := "usage-alias-" + tc.name + "-physical"
			logicalModel := "bravo/opus"
			executor := &modelExecutionCaptureExecutor{
				stream: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
					chunks := make(chan coreexecutor.StreamChunk, 1)
					chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"ok":true}`)}
					close(chunks)
					return &coreexecutor.StreamResult{Chunks: chunks}, nil
				},
			}
			handler := newModelExecutionHandler(t, physicalModel, executor, &sdkconfig.SDKConfig{})

			errMsg := tc.invoke(handler, ModelExecutionRequest{
				EntryProtocol:  "openai",
				ExitProtocol:   "openai",
				ForcedProvider: "codex",
				Model:          physicalModel,
				UsageAlias:     logicalModel,
				Body:           []byte(fmt.Sprintf(`{"model":%q}`, physicalModel)),
			})
			if errMsg != nil {
				t.Fatalf("model execution error = %+v", errMsg)
			}

			gotReq, gotOpts := executor.captured()
			if gotReq.Model != physicalModel {
				t.Fatalf("physical model = %q, want %q", gotReq.Model, physicalModel)
			}
			if gotOpts.Metadata[coreexecutor.RequestedModelMetadataKey] != logicalModel {
				t.Fatalf(
					"usage alias metadata = %#v, want %q",
					gotOpts.Metadata[coreexecutor.RequestedModelMetadataKey],
					logicalModel,
				)
			}
		})
	}
}
