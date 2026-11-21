package flows

import (
	"errors"
	"strings"
	"testing"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/livemap"
	"github.com/MowMdown/ReCON/internal/test"
)

// vehicleTest stubs the mod side: the file, what was spawned, the colour
// picked, and whether the mod is connected. It returns the player, a way
// to type a line as role, and what was said and spawned.
type vehicleTest struct {
	file      livemap.VehicleFile
	fileErr   error
	spawnErr  error
	connected bool
	pick      int
	removeErr error
	said      []string
	spawned   []string // guid|class
}

func newVehicleTest(t *testing.T) (*vehicleTest, func(role int, text string)) {
	vt := &vehicleTest{connected: true}
	oldSay, oldConn, oldChoices, oldSpawn, oldPick, oldRemove := chatSay, modConnected, vehicleChoices, spawnVehicle, pickColour, removeChatVehicle
	t.Cleanup(func() {
		chatSay, modConnected, vehicleChoices, spawnVehicle, pickColour, removeChatVehicle = oldSay, oldConn, oldChoices, oldSpawn, oldPick, oldRemove
		chatState.Lock()
		chatState.loaded = false
		chatState.Unlock()
	})
	chatSay = func(to battleye.Player, everyone bool, text string) bool {
		vt.said = append(vt.said, text)
		return true
	}
	modConnected = func() bool { return vt.connected }
	vehicleChoices = func() (livemap.VehicleFile, string, error) { return vt.file, "deerisle", vt.fileErr }
	spawnVehicle = func(guid, class string) (string, error) {
		if vt.spawnErr != nil {
			return "", vt.spawnErr
		}
		vt.spawned = append(vt.spawned, guid+"|"+class)
		return "Olga 24", nil
	}
	pickColour = func(n int) int { return vt.pick }
	removeChatVehicle = func(guid string) (string, error) {
		if vt.removeErr != nil {
			return "", vt.removeErr
		}
		vt.spawned = append(vt.spawned, "remove|"+guid)
		return "Olga 24", nil
	}
	chatState.Lock()
	chatState.settings, chatState.loaded = DefaultChatSettings(), true
	chatState.Unlock()
	p := battleye.Player{ID: 3, GUID: test.GUID1, Name: "Staff"}
	return vt, func(role int, text string) {
		vt.said, vt.spawned = nil, nil
		handleChatCommand(p, role, text)
	}
}

func eq(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

func TestVehicleCommandSpawns(t *testing.T) {
	vt, typ := newVehicleTest(t)
	vt.file = livemap.VehicleFile{"car": {"olga": {"default": "CivilianSedan", "black": "CivilianSedan_Black"}}}
	typ(roleMod, "!CAR Olga BLACK")
	eq(t, "spawned", vt.spawned, test.GUID1+"|CivilianSedan_Black")
	eq(t, "said", vt.said, "Spawning Olga 24 (black)")
	typ(roleMod, "!car olga")
	eq(t, "spawned", vt.spawned, test.GUID1+"|CivilianSedan")
	eq(t, "said", vt.said, "Spawning Olga 24 (default)")
}

func TestVehicleCommandRandomColour(t *testing.T) {
	vt, typ := newVehicleTest(t)
	vt.file = livemap.VehicleFile{"boat": {"rubber": {"blue": "Boat_01_Blue", "black": "Boat_01_Black"}}}
	vt.pick = 1
	typ(RoleAdmin, "!boat rubber")
	eq(t, "spawned", vt.spawned, test.GUID1+"|Boat_01_Blue")
}

func TestVehicleCommandLists(t *testing.T) {
	vt, typ := newVehicleTest(t)
	vt.file = livemap.VehicleFile{"car": {
		"olga":  {"default": "CivilianSedan", "black": "CivilianSedan_Black", "wine": "CivilianSedan_Wine"},
		"sarka": {"red": "Sedan_02_Red"},
	}}
	typ(roleMod, "!car")
	eq(t, "!car", vt.said, "Cars: olga (black, wine), sarka (red)")
	eq(t, "spawned", vt.spawned)
	typ(roleMod, "!car lada")
	eq(t, "!car lada", vt.said, "Cars: olga (black, wine), sarka (red)")
	typ(roleMod, "!car olga pink")
	eq(t, "!car olga pink", vt.said, "Cars: olga (black, wine)")
	typ(roleMod, "!truck")
	eq(t, "!truck", vt.said, "Trucks: none yet: add some to vehicles.json")
}

func TestVehicleCommandErrors(t *testing.T) {
	vt, typ := newVehicleTest(t)
	vt.file = livemap.VehicleFile{"car": {"olga": {"default": "CivilianSedan_Typo"}}}
	vt.spawnErr = errors.New("CivilianSedan_Typo isn't an item the server can spawn")
	typ(roleMod, "!car olga")
	eq(t, "bad class", vt.said, "CivilianSedan_Typo isn't an item the server can spawn")
	vt.fileErr = errors.New("vehicles.json for deerisle has an error at line 4: invalid character '}'")
	typ(roleMod, "!car olga")
	eq(t, "bad file", vt.said, "vehicles.json for deerisle has an error at line 4: invalid character '}'")
}

func TestVehicleCommandRolesAndMod(t *testing.T) {
	vt, typ := newVehicleTest(t)
	vt.file = livemap.VehicleFile{"car": {"olga": {"default": "CivilianSedan"}}}
	typ(RolePlayer, "!car olga")
	eq(t, "player said", vt.said)
	eq(t, "player spawned", vt.spawned)

	everyone := "!help, !shout, !whisper, !players, !vote, !code"
	vt.connected = false
	typ(roleMod, "!car olga")
	eq(t, "no mod", vt.said, "recon-mod isn't running on this server")
	eq(t, "no mod spawned", vt.spawned)
	typ(roleMod, "!help")
	eq(t, "help without mod", vt.said, "Commands: "+everyone+", !kick, !ban, !warning. Type !help !<command> for details.")
	typ(roleMod, "!help !car")
	eq(t, "help !car without mod", vt.said)

	vt.connected = true
	typ(roleMod, "!help")
	eq(t, "help with mod", vt.said, "Commands: "+everyone+", !kick, !ban, !warning, !car, !truck, !boat, !vehicle, !remove. Type !help !<command> for details.")
}

func TestRemoveCommand(t *testing.T) {
	vt, typ := newVehicleTest(t)
	typ(RoleVehicles, "!remove")
	eq(t, "removed", vt.spawned, "remove|"+test.GUID1)
	eq(t, "said", vt.said, "Removing your Olga 24")
	vt.removeErr = livemap.ErrNoChatVehicle
	typ(RoleVehicles, "!remove")
	eq(t, "none", vt.said, "You don't have a spawned vehicle")
	vt.removeErr = nil
	typ(RolePlayer, "!remove")
	eq(t, "player", vt.said)
	eq(t, "player removed", vt.spawned)
}
