package linuxvm

import "context"

func prepareAll(ctx context.Context, config PrepareConfig, concurrency int) []PrepareOutcome {
	names := make([]string, 0, len(config.Resolved.Nodes))
	for _, node := range config.Resolved.Nodes {
		names = append(names, node.Name)
	}
	return PrepareSelected(ctx, config, names, concurrency)
}

func readyNames(outcomes []StartOutcome) []string {
	result := make([]string, 0, len(outcomes))
	for _, outcome := range outcomes {
		if outcome.Ready {
			result = append(result, outcome.Node)
		}
	}
	return result
}

func runningNames(outcomes []StartOutcome) []string {
	result := make([]string, 0, len(outcomes))
	for _, outcome := range outcomes {
		if outcome.Running {
			result = append(result, outcome.Node)
		}
	}
	return result
}

func stoppedNames(outcomes []StopOutcome) []string {
	result := make([]string, 0, len(outcomes))
	for _, outcome := range outcomes {
		if outcome.Stopped {
			result = append(result, outcome.Node)
		}
	}
	return result
}
