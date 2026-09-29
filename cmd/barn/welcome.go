package main

import "io"

type welcomeAction struct {
	Label   string `json:"label"`
	Command string `json:"command"`
}

// The bare command is a small map of the next few actions. --help remains
// the complete reference.
func welcomeActions() []welcomeAction {
	var actions []welcomeAction
	if resolved, err := currentDeploymentResolved(); err == nil && len(resolved.Nodes) > 0 {
		actions = []welcomeAction{{"connect", "barn ssh"}, {"inspect", "barn status"}, {"continue", "barn up"}, {"stop", "barn stop"}}
	} else {
		actions = []welcomeAction{{"start", "barn up"}, {"customize", "barn init"}, {"preview", "barn plan"}}
	}
	if macSupported() {
		actions = append(actions, welcomeAction{"macOS", "barn mac"})
	}
	return append(actions, welcomeAction{"help", "barn --help"})
}

func printWelcome(out io.Writer, actions []welcomeAction) {
	bestEffortln(out, "Barn · local Linux virtual machines")
	bestEffortln(out)
	for _, action := range actions {
		textField(out, 10, action.Label, action.Command)
	}
}
