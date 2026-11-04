package main

type welcomeAction struct {
	Label   string `json:"label"`
	Command string `json:"command"`
}

// welcomeActions is the structured form of bare `barn`: the next few actions
// for the current deployment. The text form prints the complete help.
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
