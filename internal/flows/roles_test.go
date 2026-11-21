package flows

import (
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/livemap"
	"github.com/MowMdown/ReCON/internal/test"
)

func TestRoleRank(t *testing.T) {
	for role, rank := range map[int]int{RolePlayer: 0, RoleVehicles: 1, roleMod: 2, RoleAdmin: 3} {
		if got := RoleRank(role); got != rank {
			t.Errorf("RoleRank(%d) = %d, want %d", role, got, rank)
		}
	}
	if RoleName(RoleVehicles) != "Vehicles" || RoleMax != 3 {
		t.Errorf("name %q max %d", RoleName(RoleVehicles), RoleMax)
	}
}

func TestVehiclesRoleCommands(t *testing.T) {
	vt, typ := newVehicleTest(t)
	vt.file = livemap.VehicleFile{"car": {"olga": {"default": "CivilianSedan"}}}
	typ(RoleVehicles, "!car olga")
	eq(t, "spawned", vt.spawned, test.GUID1+"|CivilianSedan")
	typ(RoleVehicles, "!kick 5 rude")
	eq(t, "!kick said", vt.said)
	typ(RoleVehicles, "!help")
	eq(t, "!help", vt.said, "Commands: !help, !shout, !whisper, !players, !vote, !code, !car, !truck, !boat, !vehicle. Type !help !<command> for details.")
}

func TestVehiclesRoleNotStaff(t *testing.T) {
	db := testDB(t)
	const vGUID = "0000000000000000000000000000beef"
	now := time.Now()
	for _, p := range []battleye.Player{{ID: 1, GUID: vGUID, Name: "Driver"}, {ID: 2, GUID: test.GUID1, Name: "Mod"}} {
		if err := db.OpenPlayerSession(p.GUID, p.Name, "1.2.3.4", p.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SetPlayerRole(vGUID, RoleVehicles); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetPlayerRole(test.GUID1, roleMod); err != nil {
		t.Fatal(err)
	}
	setOnline(battleye.Player{ID: 1, GUID: vGUID, Name: "Driver"}, battleye.Player{ID: 2, GUID: test.GUID1, Name: "Mod"})
	staff := listOnlineStaff()
	if len(staff) != 1 || staff[0].GUID != test.GUID1 {
		t.Errorf("staff %+v, want only the Mod", staff)
	}
	mod := battleye.Player{ID: 2, GUID: test.GUID1, Name: "Mod"}
	if !chatTargetAllowed(mod, roleMod, battleye.Player{ID: 1, GUID: vGUID, Name: "Driver"}) {
		t.Error("a Mod can't punish a Vehicles player")
	}
}

func TestRoleConditionUsesRank(t *testing.T) {
	cases := []struct {
		op, value, v string
		want         bool
	}{
		{"ge", "1", "3", false}, // a Vehicles player is not at least Mod
		{"ge", "1", "1", true},
		{"ge", "1", "2", true},
		{"le", "3", "0", true}, // a Player is at most Vehicles
		{"le", "3", "1", false},
		{"ge", "3", "3", true},
		{"eq", "3", "3", true},
	}
	for _, c := range cases {
		if got := evaluateIfCondition(Branch{Subject: "player_role", Op: c.op, Value: c.value}, c.v); got != c.want {
			t.Errorf("role %s %s against %s = %v, want %v", c.op, c.value, c.v, got, c.want)
		}
	}
}
