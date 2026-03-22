package root

import (
	"cmp"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/rag"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type ragFlags struct {
	runConfig config.RuntimeConfig
}

func newRAGCmd() *cobra.Command {
	var flags ragFlags

	cmd := &cobra.Command{
		Use:     "rag <agent-file> <rag-name> <query>",
		Short:   "Test a RAG source by running a query",
		Long:    "Load a configuration file, initialize the specified RAG source, and run a query against it.",
		GroupID: "advanced",
		Args:    cobra.ExactArgs(3),
		RunE:    flags.runRAGCommand,
	}

	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *ragFlags) runRAGCommand(cmd *cobra.Command, args []string) error {
	telemetry.TrackCommand("rag", args)

	ctx := cmd.Context()

	agentSource, err := config.Resolve(args[0], f.runConfig.EnvProvider())
	if err != nil {
		return err
	}

	cfg, err := config.Load(ctx, agentSource)
	if err != nil {
		return err
	}

	parentDir := cmp.Or(agentSource.ParentDir(), f.runConfig.WorkingDir)

	managers, err := rag.NewManagers(ctx, cfg, rag.ManagersBuildConfig{
		ParentDir:     parentDir,
		ModelsGateway: f.runConfig.ModelsGateway,
		Env:           f.runConfig.EnvProvider(),
		Models:        cfg.Models,
		Providers:     cfg.Providers,
	})
	if err != nil {
		return fmt.Errorf("failed to create RAG managers: %w", err)
	}

	ragName := args[1]
	var manager *rag.Manager
	available := make([]string, 0, len(managers))
	for _, candidate := range managers {
		available = append(available, candidate.Name())
		if candidate.Name() == ragName {
			manager = candidate
		}
	}
	if manager == nil {
		return fmt.Errorf("RAG source %q not found in configuration (available: %v)", ragName, available)
	}
	defer manager.Close()

	if err := manager.Initialize(ctx); err != nil {
		return fmt.Errorf("failed to initialize RAG source %q: %w", ragName, err)
	}

	results, err := manager.Query(ctx, args[2])
	if err != nil {
		return fmt.Errorf("query failed: %w", err)
	}

	type queryResult struct {
		SourcePath string  `json:"source_path"`
		Content    string  `json:"content"`
		Similarity float64 `json:"similarity"`
		ChunkIndex int     `json:"chunk_index"`
	}

	output := make([]queryResult, 0, len(results))
	for _, r := range results {
		output = append(output, queryResult{
			SourcePath: r.Document.SourcePath,
			Content:    r.Document.Content,
			Similarity: r.Similarity,
			ChunkIndex: r.Document.ChunkIndex,
		})
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal results: %w", err)
	}

	fmt.Fprintln(cmd.OutOrStdout(), string(data))
	return nil
}
