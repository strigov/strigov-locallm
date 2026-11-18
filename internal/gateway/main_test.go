package gateway

import (
	"os"
	"testing"

	"github.com/strigov/strigov-locallm/internal/testutil"
)

// TestMain удаляет каталог со сборкой fakeupstream.exe после тестов пакета.
func TestMain(m *testing.M) { os.Exit(testutil.Main(m)) }
