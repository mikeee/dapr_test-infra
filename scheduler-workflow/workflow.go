package main

import (
	"context"
	"fmt"
	"log"
	http2 "net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/dapr/go-sdk/service/common"
	"github.com/dapr/go-sdk/service/http"
	"github.com/dapr/go-sdk/workflow"
)

const appPort = ":3009"

var stage atomic.Int64

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := dapr.NewClient()
	if err != nil {
		log.Fatalf("Error getting dapr client: %v", err)
	}
	defer client.Close()

	daprService := http.NewService(appPort)
	go func() {
		log.Println("Starting app service...")
		if err := daprService.Start(); err != nil && err.Error() != http2.ErrServerClosed.Error() {
			log.Fatalf("error starting app service: %v", err)
		}
	}()

	worker, err := workflow.NewWorker()
	if err != nil {
		log.Fatal(err)
	}
	log.Println("Workflow worker initialized")

	if err := worker.RegisterWorkflow(TestWorkflow); err != nil {
		log.Printf("Failed to register workflow: %v", err)
	}
	if err := worker.RegisterActivity(TestActivity); err != nil {
		log.Printf("Failed to register activity: %v", err)
	}

	// Start workflow runner
	if err := worker.Start(); err != nil {
		log.Printf("Failed to start worker: %v", err)
	}
	log.Println("Workflow worker started")

	go startLonghaulWorkflow(ctx, client)
	waitForShutdown(daprService, worker, cancel)
}

func TestWorkflow(ctx *workflow.WorkflowContext) (any, error) {
	var input int
	if err := ctx.GetInput(&input); err != nil {
		return nil, err
	}
	var output string
	if err := ctx.CallActivity(TestActivity, workflow.ActivityInput(input)).Await(&output); err != nil {
		return nil, err
	}

	err := ctx.WaitForExternalEvent("testEvent", time.Second*60).Await(&output)
	if err != nil {
		return nil, err
	}

	if err := ctx.CallActivity(TestActivity, workflow.ActivityInput(input)).Await(&output); err != nil {
		return nil, err
	}

	return output, nil
}

func TestActivity(ctx workflow.ActivityContext) (any, error) {
	var input int
	if err := ctx.GetInput(&input); err != nil {
		return "", err
	}

	if stage.Load() >= 100 {
		stage.Store(0)
	}

	stage.Add(1)
	return fmt.Sprintf("Stage: %d", stage.Load()), nil
}

// startLonghaulWorkflow performs the following operations on a workflow:
// start, pause, resume, raise event, terminate, purge
func startLonghaulWorkflow(ctx context.Context, client dapr.Client) {
	i := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
			log.Printf("Starting workflow iteration %d\n", i)
			instanceID := fmt.Sprintf("longhaul-instance-%d", i)
			workflowReq := &dapr.StartWorkflowRequest{
				InstanceID:        instanceID,
				WorkflowComponent: "",
				WorkflowName:      "TestWorkflow",
				Input:             i,
				SendRawInput:      false,
			}

			startWfCtx, startWfCancel := context.WithTimeout(ctx, 5*time.Second)
			respStart, err := client.StartWorkflowBeta1(startWfCtx, workflowReq)
			startWfCancel()
			if err != nil {
				log.Printf("Iteration %d: Failed to start workflow: %v\n", i, err)
				continue
			}
			log.Printf("Workflow started with ID: '%s'\n", respStart.InstanceID)

			pauseWfCtx, pauseWfCancel := context.WithTimeout(ctx, 5*time.Second)
			err = client.PauseWorkflowBeta1(pauseWfCtx, &dapr.PauseWorkflowRequest{
				InstanceID:        instanceID,
				WorkflowComponent: "",
			})
			pauseWfCancel()
			if err != nil {
				log.Printf("Failed to pause workflow: %v\n", err)
			}
			log.Printf("Workflow '%s' paused\n", instanceID)
			resumeWfCtx, resumeWfCancel := context.WithTimeout(ctx, 5*time.Second)
			err = client.ResumeWorkflowBeta1(resumeWfCtx, &dapr.ResumeWorkflowRequest{
				InstanceID:        instanceID,
				WorkflowComponent: "",
			})
			resumeWfCancel()
			if err != nil {
				log.Printf("Failed to resume workflow: %v\n", err)
			}
			log.Printf("Workflow '%s' resumed\n", instanceID)
			raiseEventWfCtx, raiseEventWfCancel := context.WithTimeout(ctx, 5*time.Second)
			// Raise event to advance the workflow
			err = client.RaiseEventWorkflowBeta1(raiseEventWfCtx, &dapr.RaiseEventWorkflowRequest{
				InstanceID:        instanceID,
				WorkflowComponent: "",
				EventName:         "testEvent",
				EventData:         "testData",
			})
			raiseEventWfCancel()
			if err != nil {
				log.Printf("Failed to raise event: %v\n", err)
			}
			log.Printf("Workflow '%s' event raised\n", instanceID)
			log.Printf("[wfclient] stage: %d\n", stage.Load())

			// Wait for workflow to complete
			// Poll every 5 seconds to check the workflow status
			waitForWorkflowCompletion(ctx, client, instanceID)
			terminateWfCtx, terminateWfCancel := context.WithTimeout(ctx, 5*time.Second)
			// Terminate and purge after completion
			err = client.TerminateWorkflowBeta1(terminateWfCtx, &dapr.TerminateWorkflowRequest{
				InstanceID: instanceID,
			})
			terminateWfCancel()
			if err != nil {
				log.Printf("Failed to terminate workflow %s: %v\n", instanceID, err)
			} else {
				log.Printf("Workflow '%s' terminated\n", instanceID)
			}
			purgeWfCtx, purgeWfCancel := context.WithTimeout(ctx, 5*time.Second)
			err = client.PurgeWorkflowBeta1(purgeWfCtx, &dapr.PurgeWorkflowRequest{
				InstanceID: instanceID,
			})
			purgeWfCancel()
			if err != nil {
				log.Printf("Failed to purge workflow %s: %v\n", instanceID, err)
			} else {
				log.Printf("Workflow '%s' purged\n", instanceID)
			}

			i++
			if i >= 100 {
				i = 0
			}
		}
	}
}

// waitForWorkflowCompletion polls every 5s to check the workflow status
func waitForWorkflowCompletion(ctx context.Context, client dapr.Client, instanceID string) {
	for {
		respGet, err := client.GetWorkflowBeta1(ctx, &dapr.GetWorkflowRequest{
			InstanceID: instanceID,
		})
		if err != nil {
			log.Printf("Error retrieving workflow status for %s: %v\n", instanceID, err)
			continue
		}

		switch respGet.RuntimeStatus {
		case workflow.StatusCompleted.String():
			log.Printf("Workflow '%s' completed\n", instanceID)
			return
		case workflow.StatusFailed.String():
			log.Printf("Workflow '%s' failed\n", instanceID)
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// waitForShutdown keeps the app alive until an interrupt or termination signal is received
func waitForShutdown(daprService common.Service, worker *workflow.WorkflowWorker, cancelFunc context.CancelFunc) {
	sigCh := make(chan os.Signal, 1)
	// Notify the channel on Interrupt (Ctrl+C) or SIGTERM (for Docker/K8s graceful shutdown)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")

	if err := daprService.GracefulStop(); err != nil {
		log.Printf("Failed to gracefully shutdown dapr service: %v", err)
	}

	if err := worker.Shutdown(); err != nil {
		log.Printf("Failed to shutdown workflow worker: %v", err)
	}

	cancelFunc()
}
