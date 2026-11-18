package supervisor

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

func TestMain(m *testing.M) {
	switch os.Getenv("LOCALLM_TEST_HELPER") {
	case "jobhost":
		os.Exit(jobHost())
	case "jobflags":
		os.Exit(jobFlagsHelper())
	}
	os.Exit(testutil.Main(m))
}

// jobHost — процесс-«хост» для теста Job Object и S1: входит в job, запускает дочерний процесс
// через Supervisor.Start, печатает "CHILD <pid>" и ждёт, пока его убьют.
func jobHost() int {
	if err := EnterJob(); err != nil {
		fmt.Println("ERR", err)
		return 3
	}
	port, _ := strconv.Atoi(os.Getenv("LOCALLM_TEST_PORT"))
	var args []string
	if a := os.Getenv("LOCALLM_TEST_ARGS"); a != "" {
		args = strings.Split(a, "|")
	}
	cfg := &config.Config{Llama: config.Llama{ServerExe: os.Getenv("LOCALLM_TEST_EXE"), Port: port,
		LoadTimeout: 300 * time.Second, StopTimeout: 10 * time.Second, LogLines: 100}}
	up, err := New(cfg, nil).Start(context.Background(), config.Model{ID: "child", Path: os.Getenv("LOCALLM_TEST_MODEL"), Args: args})
	if err != nil {
		fmt.Println("ERR", err)
		return 4
	}
	fmt.Printf("CHILD %d\n", up.PID)
	select {}
}

// jobFlagsHelper — процесс для TestEnterJobFlags: входит в job хоста и печатает "FLAGS <LimitFlags>".
func jobFlagsHelper() int {
	if err := EnterJob(); err != nil {
		fmt.Println("ERR", err)
		return 3
	}
	flags, err := jobLimitFlags()
	if err != nil {
		fmt.Println("ERR", err)
		return 4
	}
	fmt.Printf("FLAGS %#x\n", flags)
	return 0
}
