package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/klog/v2"
)

var follow bool

var logsCmd = &cobra.Command{
	Use:   "logs <sandbox-name> [-f]",
	Short: "Stream sandbox logs",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]
		namespace := viper.GetString("namespace")
		klog.V(4).InfoS("CLI logs command started", "name", name, "namespace", namespace, "follow", follow)

		client, conn := getClient()
		if conn != nil {
			defer conn.Close()
		}

		klog.V(4).InfoS("Getting sandbox info for logs", "name", name)
		info, err := client.GetSandbox(context.Background(), &fastpathv1.GetRequest{
			SandboxName: name,
			Namespace:   namespace,
		})
		if err != nil {
			klog.ErrorS(err, "Failed to get sandbox info", "name", name)
			log.Fatalf("Failed to get sandbox info: %v", err)
		}

		if info.AgentPod == "" {
			klog.ErrorS(nil, "Sandbox not assigned to any agent", "name", name)
			log.Fatal("Sandbox is not assigned to any agent yet.")
		}
		if info.SandboxId == "" {
			klog.ErrorS(nil, "Sandbox ID is not available yet", "name", name)
			log.Fatal("Sandbox ID is not available yet.")
		}
		klog.V(4).InfoS("Sandbox agent pod", "name", name, "agentPod", info.AgentPod)

		// todo add proxy for agent
		localPort, pfCmd, err := startPortForward(info.AgentPod, namespace)
		if err != nil {
			klog.ErrorS(err, "Failed to start port-forward", "agentPod", info.AgentPod)
			log.Fatalf("Failed to start port-forward: %v", err)
		}
		klog.V(4).InfoS("Port-forward started", "localPort", localPort, "agentPod", info.AgentPod)
		defer func() {
			if pfCmd != nil && pfCmd.Process != nil {
				_ = pfCmd.Process.Kill()
				_ = pfCmd.Wait()
			}
		}()

		// Use the actual sandboxID (hash) instead of name for Agent API
		url := fmt.Sprintf("http://localhost:%d/api/v1/agent/logs?sandboxId=%s&follow=%t", localPort, info.SandboxId, follow)
		klog.InfoS("Fetching logs from agent", "sandboxID", info.SandboxId, "url", url)
		reqCtx := context.Background()
		cancel := func() {}
		if !follow {
			reqCtx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		}
		defer cancel()

		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
		if err != nil {
			klog.ErrorS(err, "Failed to create logs request", "url", url)
			log.Fatalf("Failed to create logs request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			klog.ErrorS(err, "Failed to connect to agent", "url", url)
			log.Fatalf("Failed to connect to agent: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			klog.ErrorS(nil, "Agent returned error", "statusCode", resp.StatusCode, "body", string(body))
			log.Fatalf("Agent returned error: %s", string(body))
		}

		go func() {
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt)
			<-sigCh
			resp.Body.Close()
		}()

		if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
			if err != io.EOF && !errors.Is(err, io.ErrUnexpectedEOF) {
				klog.ErrorS(err, "Log stream ended with error")
				log.Printf("Log stream ended: %v", err)
			}
		}
		klog.V(4).InfoS("Log streaming completed", "name", name)
	},
}

func init() {
	rootCmd.AddCommand(logsCmd)
	logsCmd.Flags().BoolVarP(&follow, "follow", "f", false, "Specify if the logs should be streamed")
}

// startPortForward start kubectl port-forward
func startPortForward(podName, namespace string) (int, *exec.Cmd, error) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, nil, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	fmt.Printf("Forwarding local port %d to pod %s...\n", port, podName)

	// todo change port
	cmd := newPortForwardCommand(podName, namespace, port)

	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}

	for i := 0; i < 50; i++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return port, cmd, nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	cmd.Process.Kill()
	return 0, nil, fmt.Errorf("timed out waiting for port-forward")
}

func newPortForwardCommand(podName, namespace string, port int) *exec.Cmd {
	cmd := exec.Command("kubectl", "port-forward", fmt.Sprintf("pod/%s", podName), fmt.Sprintf("%d:5758", port), "-n", namespace)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd
}
