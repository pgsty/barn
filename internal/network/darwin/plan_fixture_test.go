package darwin

import "github.com/pgsty/barn/internal/network/subnet"

func NewInstallPlan(arch, interfaceID string) (InstallPlan, error) {
	return NewInstallPlanMode(arch, interfaceID, "host")
}

func NewInstallPlanMode(arch, interfaceID, mode string) (InstallPlan, error) {
	return NewInstallPlanModeNetwork(arch, interfaceID, mode, subnet.DefaultCIDR)
}
