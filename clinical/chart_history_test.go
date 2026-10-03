package clinical

import (
	"testing"
	"time"
)

func TestChartResultVersionsAreIndependent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	actor := InternalActor("dr-1")

	p, err := s.RegisterPatient(actor, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := s.AddEncounter(actor, p.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateDraft(actor, p.ID, enc.ID, Diagnosis, "initial dx")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(actor, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CorrectRecord(actor, rec.ID, 1, "corrected dx", "typo"); err != nil {
		t.Fatal(err)
	}

	// First chart view; tamper with its version id list.
	c1, err := s.Chart(actor, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(c1.Records) != 1 || len(c1.Records[0].Record.Versions) != 2 {
		t.Fatalf("unexpected history: %+v", c1.Records)
	}
	orig := append([]ID(nil), c1.Records[0].Record.Versions...)
	c1.Records[0].Record.Versions[0] = ""
	c1.Records[0].Record.Versions[1] = "ver_other_record"

	// Second chart view must be unaffected.
	c2, err := s.Chart(actor, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := c2.Records[0].Record.Versions
	if len(got) != 2 || got[0] != orig[0] || got[1] != orig[1] {
		t.Fatalf("official history changed by local mutation: %v vs %v", got, orig)
	}
	if len(c2.Records[0].Versions) != 2 {
		t.Fatalf("versions missing: %+v", c2.Records[0].Versions)
	}

	// EncounterRecords view must also be unaffected.
	er, err := s.EncounterRecords(actor, p.ID, enc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := er[0].Record.Versions; got[0] != orig[0] || got[1] != orig[1] {
		t.Fatalf("encounter view corrupted: %v", got)
	}

	// Legal correction still works and preserves history.
	v3, err := s.CorrectRecord(actor, rec.ID, 2, "final dx", "clarification")
	if err != nil {
		t.Fatal(err)
	}
	if v3.Number != 3 {
		t.Fatalf("expected version 3, got %d", v3.Number)
	}
	c3, err := s.Chart(actor, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(c3.Records[0].Versions) != 3 {
		t.Fatalf("history lost after correction: %+v", c3.Records[0].Versions)
	}
	// Stale version correction still conflicts.
	if _, err := s.CorrectRecord(actor, rec.ID, 2, "x", "y"); err == nil {
		t.Fatal("expected ErrConflict")
	}
}
