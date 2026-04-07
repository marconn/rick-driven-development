package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/marconn/rick-event-driven-development/internal/event"
	grpchandler "github.com/marconn/rick-event-driven-development/internal/grpchandler"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
)

type planOpts struct {
	grpcAddr string
	page     string // Confluence page ID or URL
	ticket   string // optional ticket reference (BTU-XXXX)
}

func newPlanCmd() *cobra.Command {
	opts := &planOpts{}

	cmd := &cobra.Command{
		Use:   "plan [prompt]",
		Short: "Start a BTU technical planning workflow",
		Long: `Read a Confluence BTU page, research the codebase, generate a technical
implementation plan with Fibonacci story point estimates, and write it back to
Confluence. Requires 'rick serve' running with the planning plugin connected.

Examples:
  rick plan --page 1994031125
  rick plan --page 1994031125 --ticket BTU-1724
  rick plan --page "https://example.atlassian.net/wiki/spaces/ING/pages/1994031125"
  rick plan "Plan insurance upload feature" --page 1994031125`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.page == "" {
				return fmt.Errorf("--page flag is required (Confluence page ID or URL)")
			}
			var prompt string
			if len(args) > 0 {
				prompt = args[0]
			}
			return runPlan(cmd.Context(), opts, prompt)
		},
	}

	cmd.Flags().StringVar(&opts.grpcAddr, "grpc-addr", "localhost:59077", "Rick gRPC server address")
	cmd.Flags().StringVar(&opts.page, "page", "", "Confluence page ID or URL (required)")
	cmd.Flags().StringVar(&opts.ticket, "ticket", "", "Ticket reference (e.g., BTU-1724)")

	return cmd
}

func runPlan(ctx context.Context, opts *planOpts, prompt string) error {
	// Parse page ID from URL if needed
	pageID := opts.page
	if strings.Contains(pageID, "/pages/") {
		parts := strings.Split(pageID, "/pages/")
		if len(parts) == 2 {
			pageID = strings.SplitN(parts[1], "/", 2)[0]
		}
	}
	sourceRef := "confluence:" + pageID

	if prompt == "" {
		prompt = fmt.Sprintf("Generate technical implementation plan for Confluence page %s", pageID)
	}

	correlationID := uuid.New().String()

	// Connect to Rick gRPC server
	conn, err := grpc.NewClient(opts.grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("connect to rick: %w", err)
	}
	defer conn.Close()

	// Build payload
	payload, err := json.Marshal(event.WorkflowRequestedPayload{
		Prompt:     prompt,
		WorkflowID: "plan-btu",
		Source:     sourceRef,
		Ticket:     opts.ticket,
	})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	// Use a temporary gRPC client to inject the event
	client := grpchandler.NewClient(conn, grpchandler.ClientConfig{
		Name:       "plan-trigger-" + correlationID[:8],
		EventTypes: []string{},
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return nil, nil
		},
		NotificationHandler: func(_ context.Context, notif *pb.WorkflowNotification) {
			_, _ = fmt.Fprintf(os.Stdout, "\nWorkflow %s: %s\n", notif.CorrelationId[:8], notif.Status)
		},
		WatchCorrelations: []string{correlationID},
		MaxRetries:        1,
	})

	// Start client in background
	clientCtx, clientCancel := context.WithCancel(ctx)
	defer clientCancel()

	clientReady := make(chan struct{})
	go func() {
		// Signal ready after short delay for registration
		go func() {
			time.Sleep(500 * time.Millisecond)
			close(clientReady)
		}()
		_ = client.Run(clientCtx)
	}()

	// Wait for client to register
	select {
	case <-clientReady:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Inject WorkflowRequested
	eventID, err := client.InjectEvent(ctx, correlationID, event.WorkflowRequested, payload)
	if err != nil {
		return fmt.Errorf("inject workflow: %w", err)
	}

	_, _ = fmt.Fprintf(os.Stdout, "plan-btu workflow started\n")
	_, _ = fmt.Fprintf(os.Stdout, "  workflow:    %s\n", correlationID)
	_, _ = fmt.Fprintf(os.Stdout, "  event:       %s\n", eventID)
	_, _ = fmt.Fprintf(os.Stdout, "  page:        %s\n", pageID)
	if opts.ticket != "" {
		_, _ = fmt.Fprintf(os.Stdout, "  ticket:      %s\n", opts.ticket)
	}
	_, _ = fmt.Fprintf(os.Stdout, "\nMonitor with:\n")
	_, _ = fmt.Fprintf(os.Stdout, "  rick status %s\n", correlationID)
	_, _ = fmt.Fprintf(os.Stdout, "  rick events --correlation %s\n", correlationID)
	_, _ = fmt.Fprintf(os.Stdout, "\nWhen paused for review, approve or adjust with:\n")
	_, _ = fmt.Fprintf(os.Stdout, "  rick resume %s\n", correlationID)
	_, _ = fmt.Fprintf(os.Stdout, "  rick guide %s \"adjust task X to include...\"\n", correlationID)

	// Disconnect
	clientCancel()
	return nil
}
