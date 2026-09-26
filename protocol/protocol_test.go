package protocol_test

// Importing every generated package runs its init, which builds the
// libwayland interface tables and panics on any inconsistency.
import (
	"testing"

	_ "github.com/bnema/purego-libwayland/protocol/committiming"
	_ "github.com/bnema/purego-libwayland/protocol/contenttype"
	_ "github.com/bnema/purego-libwayland/protocol/cursorshape"
	_ "github.com/bnema/purego-libwayland/protocol/extdatacontrol"
	_ "github.com/bnema/purego-libwayland/protocol/extforeigntoplevellist"
	_ "github.com/bnema/purego-libwayland/protocol/extidlenotify"
	_ "github.com/bnema/purego-libwayland/protocol/extsessionlock"
	_ "github.com/bnema/purego-libwayland/protocol/fifo"
	_ "github.com/bnema/purego-libwayland/protocol/fractionalscale"
	_ "github.com/bnema/purego-libwayland/protocol/idleinhibit"
	_ "github.com/bnema/purego-libwayland/protocol/kdedecoration"
	_ "github.com/bnema/purego-libwayland/protocol/keyboardshortcutsinhibit"
	_ "github.com/bnema/purego-libwayland/protocol/linuxdmabuf"
	_ "github.com/bnema/purego-libwayland/protocol/linuxdrmsyncobj"
	_ "github.com/bnema/purego-libwayland/protocol/pointerconstraints"
	_ "github.com/bnema/purego-libwayland/protocol/pointergestures"
	_ "github.com/bnema/purego-libwayland/protocol/presentationtime"
	_ "github.com/bnema/purego-libwayland/protocol/primaryselection"
	_ "github.com/bnema/purego-libwayland/protocol/relativepointer"
	_ "github.com/bnema/purego-libwayland/protocol/tabletv2"
	_ "github.com/bnema/purego-libwayland/protocol/tearingcontrol"
	_ "github.com/bnema/purego-libwayland/protocol/viewporter"
	_ "github.com/bnema/purego-libwayland/protocol/virtualkeyboard"
	_ "github.com/bnema/purego-libwayland/protocol/wayland"
	_ "github.com/bnema/purego-libwayland/protocol/wlrlayershell"
	_ "github.com/bnema/purego-libwayland/protocol/xdgactivation"
	_ "github.com/bnema/purego-libwayland/protocol/xdgdecoration"
	_ "github.com/bnema/purego-libwayland/protocol/xdgdialog"
	_ "github.com/bnema/purego-libwayland/protocol/xdgforeign"
	_ "github.com/bnema/purego-libwayland/protocol/xdgoutput"
	_ "github.com/bnema/purego-libwayland/protocol/xdgshell"
	_ "github.com/bnema/purego-libwayland/protocol/xdgsystembell"
	_ "github.com/bnema/purego-libwayland/protocol/xdgtoplevelicon"
	_ "github.com/bnema/purego-libwayland/protocol/xdgtopleveltag"
)

func TestGeneratedPackagesInit(t *testing.T) {}
