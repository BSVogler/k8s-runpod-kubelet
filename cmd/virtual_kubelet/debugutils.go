package main

import (
	"fmt"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// DebugWaitForNamedCacheSync is an enhanced version of WaitForNamedCacheSync
// that provides additional debugging information
func DebugWaitForNamedCacheSync(controllerName string, stopCh <-chan struct{}, cacheSyncs ...cache.InformerSynced) bool {
	klog.Infof("DEBUG: Waiting for caches to sync for %s", controllerName)

	// Create a map to track which cache sync functions aren't ready yet
	cacheSyncStatus := make(map[int]bool)
	for i := range cacheSyncs {
		cacheSyncStatus[i] = false
	}

	// Print debug info about HasSynced
	for i, syncFunc := range cacheSyncs {
		// Try to print the function value
		klog.Infof("DEBUG: Cache sync function %d (%v) for %s starting to sync",
			i, syncFunc, controllerName)

		// Try to execute it once with panic recovery to see if it panics
		func() {
			defer func() {
				if r := recover(); r != nil {
					klog.Errorf("DEBUG: Panic in sync function %d: %v", i, r)
				}
			}()

			result := syncFunc()
			klog.Infof("DEBUG: Initial sync function %d result: %v", i, result)
		}()
	}

	// Use a timer to periodically log progress
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Timeout after 30 seconds (adjust as needed)
	timeout := time.After(30 * time.Second)

	// Create a channel for poll results
	doneCh := make(chan bool)

	// Start polling in a goroutine
	go func() {
		for {
			allSynced := true
			for i, syncFunc := range cacheSyncs {
				isSynced := syncFunc()
				if isSynced != cacheSyncStatus[i] {
					cacheSyncStatus[i] = isSynced
					klog.Infof("DEBUG: Cache sync %d for %s changed state: %v",
						i, controllerName, isSynced)
				}
				if !isSynced {
					allSynced = false
				}
			}

			if allSynced {
				doneCh <- true
				return
			}

			// Check if we should exit
			select {
			case <-stopCh:
				return
			default:
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()

	// Wait for completion, timeout, or periodic logging
	for {
		select {
		case <-doneCh:
			klog.Infof("DEBUG: All caches synced for %s", controllerName)
			return true
		case <-timeout:
			klog.Errorf("DEBUG: Timeout waiting for caches to sync for %s.", controllerName)

			// Try to get more detailed information about what's not syncing
			for i, ready := range cacheSyncStatus {
				if !ready {
					// Try to introspect more about this informer if possible
					klog.Errorf("DEBUG: Cache sync %d failed to sync. Possible cause: informer not receiving events from API server or slow processing", i)
				}
			}

			// Try to get resource version info if we can
			klog.Errorf("DEBUG: Check API server connection and watch events. Run 'kubectl get events' to see if there are API server issues")

			utilruntime.HandleError(fmt.Errorf("unable to sync caches for %s", controllerName))
			return false
		case <-ticker.C:
			// Log periodic updates
			klog.Infof("DEBUG: Still waiting for caches to sync for %s. Current status:", controllerName)
			for i, ready := range cacheSyncStatus {
				klog.Infof("DEBUG: Cache sync %d: %v", i, ready)
			}
		case <-stopCh:
			klog.Infof("DEBUG: Stop channel closed while waiting for caches to sync")
			return false
		}
	}
}
