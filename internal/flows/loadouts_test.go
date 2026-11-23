package flows

import (
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/livemap"
	"github.com/MowMdown/ReCON/internal/test"
)

func TestEquipConfirm(t *testing.T) {
	vt, typ := newVehicleTest(t)
	oldCount, oldEquip, oldFor := loadoutCount, equipLoadout, equipConfirmFor
	t.Cleanup(func() { loadoutCount, equipLoadout, equipConfirmFor = oldCount, oldEquip, oldFor })
	have, equipped := false, 0
	loadoutCount = func(string) (int, error) {
		if !have {
			return 0, livemap.ErrNoLoadout
		}
		return 34, nil
	}
	equipLoadout = func(string) (int, error) { equipped++; return 34, nil }
	p := battleye.Player{ID: 3, GUID: test.GUID1, Name: "Staff"}
	answer := func(text string) bool {
		vt.said = nil
		return answerEquip(p, text)
	}

	typ(RolePlayer, "!equip")
	eq(t, "player", vt.said)
	typ(roleMod, "!equip")
	eq(t, "no loadout", vt.said, "No loadout saved, use !save first")

	have = true
	typ(roleMod, "!equip")
	eq(t, "asks", vt.said, "Replace your gear with your saved loadout (34 items)? Type confirm within 30 s")
	if !answer("  Confirm ") || equipped != 1 {
		t.Fatalf("confirm didn't equip (equipped %d)", equipped)
	}
	eq(t, "equipped", vt.said, "Loadout equipped: 34 items")
	if answer("confirm") || equipped != 1 {
		t.Fatal("a second confirm equipped again")
	}

	typ(roleMod, "!equip")
	if !answer("no thanks") || equipped != 1 {
		t.Fatal("another message didn't cancel")
	}
	eq(t, "cancelled", vt.said, "Loadout not equipped")

	equipConfirmFor = 50 * time.Millisecond
	typ(roleMod, "!equip")
	vt.said = nil
	time.Sleep(300 * time.Millisecond)
	eq(t, "timed out", vt.said, "Loadout not equipped")
	if answer("confirm") || equipped != 1 {
		t.Fatal("confirm after the time equipped")
	}
}
