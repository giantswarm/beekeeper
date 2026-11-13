package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/giantswarm/beekeeper/internal/apiread"
	"github.com/giantswarm/beekeeper/internal/proc"
)

func (a *app) apiReadCmd() *cobra.Command {
	var duration time.Duration
	var data string
	c := &cobra.Command{
		Use:   "api-read <api> <service/method>",
		Short: "Call a configured read RPC with grpcurl and a private short-lived token",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, ok := a.cfg.Kube.APIReads[args[0]]
			if !ok || p.Context == "" || p.Namespace == "" || p.ServiceAccount == "" || p.Audience == "" || p.Endpoint == "" || strings.HasPrefix(p.Endpoint, "-") || !slices.Contains(p.Methods, args[1]) {
				return refused("API reader is incomplete or RPC is not configured as read-only")
			}
			if duration < apiread.MinDuration || duration > apiread.MaxDuration {
				return usageErr("duration must be between 10m and 15m")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), duration)
			defer cancel()
			rules := clientcmd.NewDefaultClientConfigLoadingRules()
			if files := a.kubeconfigFiles(); len(files) > 0 {
				rules.Precedence = files
			}
			cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: p.Context}).ClientConfig()
			if err != nil {
				return refused("cannot load API reader kubeconfig")
			}
			if cfg.Insecure {
				return refused("API reader requires verified Kubernetes TLS")
			}
			client, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return refused("cannot initialize API reader")
			}
			token, err := apiread.Token(ctx, client, p.Namespace, p.ServiceAccount, p.Audience, duration)
			if err != nil {
				return refused("%v", err)
			}
			return runReadRPC(ctx, p.Endpoint, args[1], data, token, a.out)
		},
	}
	c.Flags().DurationVar(&duration, "duration", 10*time.Minute, "token lifetime (10m through 15m)")
	c.Flags().StringVar(&data, "data", "{}", "JSON request body")
	return c
}

// Keep tokens out of argv and buffer output to redact across write boundaries.
// Only fixed grpcurl options are accepted, with TLS verification enabled.
func runReadRPC(ctx context.Context, endpoint, method, data, token string, out io.Writer) error {
	c := proc.CommandContext(ctx, "grpcurl", "-expand-headers", "-H", "Authorization: Bearer ${BEEKEEPER_API_READ_TOKEN}", "-d", data, endpoint, method) //nolint:gosec // fixed binary and options; the caller checks endpoint and method against the profile
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "BEEKEEPER_API_READ_TOKEN=") {
			c.Env = append(c.Env, entry)
		}
	}
	c.Env = append(c.Env, "BEEKEEPER_API_READ_TOKEN="+token)
	var output limitedRPCOutput
	c.Stdout, c.Stderr = &output, &output
	err := c.Run()
	if err != nil {
		return fmt.Errorf("grpcurl read failed")
	}
	text := output.String()
	for _, value := range []string{token, base64.StdEncoding.EncodeToString([]byte(token)), base64.RawURLEncoding.EncodeToString([]byte(token))} {
		text = strings.ReplaceAll(text, value, "[REDACTED]")
	}
	if _, writeErr := io.WriteString(out, text); writeErr != nil {
		return writeErr
	}
	return nil
}

type limitedRPCOutput struct{ data bytes.Buffer }

func (b *limitedRPCOutput) String() string { return b.data.String() }

func (b *limitedRPCOutput) Write(p []byte) (int, error) {
	if b.data.Len()+len(p) > 16<<20 {
		return 0, fmt.Errorf("RPC output exceeds 16 MiB")
	}
	return b.data.Write(p)
}
