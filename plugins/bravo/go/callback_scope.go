package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func forkHostCallbackScope(parentID string) (string, error) {
	parentID = strings.TrimSpace(parentID)
	if parentID == "" {
		return "", fmt.Errorf("host callback parent id is required")
	}
	raw, errCall := callHost(pluginabi.MethodHostCallbackFork, pluginapi.HostCallbackScopeRequest{
		HostCallbackID: parentID,
	})
	if errCall != nil {
		return "", errCall
	}
	var response pluginapi.HostCallbackScopeResponse
	if errDecode := json.Unmarshal(raw, &response); errDecode != nil {
		return "", fmt.Errorf("decode forked host callback scope: %w", errDecode)
	}
	childID := strings.TrimSpace(response.HostCallbackID)
	if childID == "" {
		return "", fmt.Errorf("host returned an empty child callback id")
	}
	return childID, nil
}

func closeHostCallbackScope(callbackID string) error {
	callbackID = strings.TrimSpace(callbackID)
	if callbackID == "" {
		return nil
	}
	_, errCall := callHost(pluginabi.MethodHostCallbackClose, pluginapi.HostCallbackScopeRequest{
		HostCallbackID: callbackID,
	})
	return errCall
}

func commitHostCallbackScope(callbackID string) error {
	callbackID = strings.TrimSpace(callbackID)
	if callbackID == "" {
		return fmt.Errorf("host callback id is required")
	}
	_, errCall := callHost(pluginabi.MethodHostCallbackCommit, pluginapi.HostCallbackScopeRequest{
		HostCallbackID: callbackID,
	})
	return errCall
}
