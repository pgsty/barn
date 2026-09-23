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
		actions = []welcomeAction{{"connect", "farrow ssh"}, {"inspect", "farrow status"}, {"continue", "farrow up"}, {"stop", "farrow stop"}}
	} else {
		actions = []welcomeAction{{"start", "farrow up"}, {"customize", "farrow init"}, {"preview", "farrow plan"}}
	}
	return append(actions, welcomeAction{"help", "farrow --help"})
}

func printWelcome(out io.Writer, actions []welcomeAction) {
	bestEffortln(out, "Farrow · local Linux virtual machines")
	bestEffortln(out)
	for _, action := range actions {
		textField(out, 10, action.Label, action.Command)
	}
}
