package k8s

import (
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// SetKillGraceForTest sets how long sb's Exec waits past a command's deadline
// before it stops waiting and reports the timeout on its own authority. A live row
// that pins what Exec makes of a command that *exits* lengthens it, so a loaded
// cluster cannot push the command past Exec's bound and onto the give-up path
// instead. Test binary only.
func SetKillGraceForTest(sb sandbox.Sandbox, grace time.Duration) { sb.(*pod).killGrace = grace }
