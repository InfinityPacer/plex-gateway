package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/probe"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "plex-probe:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("plex-probe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	plexURL := flags.String("plex-url", strings.TrimSpace(os.Getenv("PLEX_URL")), "Plex base URL; defaults to PLEX_URL")
	ratingKey := flags.String("rating-key", "", "Plex metadata rating key")
	baselinePath := flags.String("baseline", "", "optional prior JSON report for Part stability comparison")
	outputPath := flags.String("output", "", "optional output JSON path; stdout when empty")
	timeout := flags.Duration("timeout", 30*time.Second, "overall Plex request timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*plexURL) == "" {
		return fmt.Errorf("-plex-url or PLEX_URL is required")
	}
	if strings.TrimSpace(*ratingKey) == "" {
		return fmt.Errorf("-rating-key is required")
	}

	client, err := probe.NewClient(*plexURL, os.Getenv("PLEX_TOKEN"), *timeout)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, err := client.FetchMetadata(ctx, *ratingKey)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*baselinePath) != "" {
		baseline, err := probe.LoadReport(*baselinePath)
		if err != nil {
			return err
		}
		comparison := probe.Compare(baseline, report)
		report.Comparison = &comparison
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	data = append(data, '\n')
	if strings.TrimSpace(*outputPath) == "" {
		_, err = stdout.Write(data)
		return err
	}
	if err := os.WriteFile(*outputPath, data, 0o600); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "wrote %s with %d Part records\n", *outputPath, len(report.Parts))
	return err
}
