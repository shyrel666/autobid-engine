// upgrade-drill delegates to the test entry so every real Office launch holds
// the same machine-wide officetest lock as all other test packages.
package main

import (
	"fmt"
	"os"
	"os/exec"
)

func main() {
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "用法：upgrade-drill（在仓库根目录运行；产物位于 data/s7-checks/upgrade）")
		os.Exit(2)
	}
	cmd := exec.Command("go", "test", "-count=1", "-timeout", "30m", "-v", "-run", "^TestUpgradeDrill$", "./cmd/upgrade-drill/")
	cmd.Env = append(os.Environ(), "AUTOBID_S7_DRILL=1", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}
