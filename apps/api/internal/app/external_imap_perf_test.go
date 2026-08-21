package app

import (
	"reflect"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestBoundedSearchUIDsOldestPreservesGapsAndLimit(t *testing.T) {
	var set imap.UIDSet
	set.AddRange(2, 4)
	set.AddNum(1000)
	set.AddRange(2000, 3000)

	got := boundedSearchUIDs(set, 5, false)
	want := []imap.UID{2, 3, 4, 1000, 2000}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("boundedSearchUIDs oldest = %v, want %v", got, want)
	}
}

func TestBoundedSearchUIDsNewestDoesNotExpandLargeRange(t *testing.T) {
	var set imap.UIDSet
	set.AddRange(1, 1_000_000_000)

	got := boundedSearchUIDs(set, 3, true)
	want := []imap.UID{1_000_000_000, 999_999_999, 999_999_998}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("boundedSearchUIDs newest = %v, want %v", got, want)
	}
}

func TestBoundedSearchUIDsRejectsDynamicAndNonUIDSets(t *testing.T) {
	var dynamic imap.UIDSet
	dynamic.AddRange(5, 0)
	if got := boundedSearchUIDs(dynamic, 10, false); len(got) != 0 {
		t.Fatalf("dynamic UID set = %v, want empty", got)
	}

	var seqSet imap.SeqSet
	seqSet.AddRange(1, 10)
	if got := boundedSearchUIDs(seqSet, 10, false); len(got) != 0 {
		t.Fatalf("sequence set = %v, want empty", got)
	}
}

func TestStaticUIDSetCount(t *testing.T) {
	var set imap.UIDSet
	set.AddRange(1, 10)
	set.AddRange(20, 24)
	if got := staticUIDSetCount(set); got != 15 {
		t.Fatalf("staticUIDSetCount=%d, want 15", got)
	}
}
