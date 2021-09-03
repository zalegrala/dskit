package client

import (
	"testing"

	"github.com/prometheus/prometheus/pkg/labels"
)

// This test shows label sets with same fingerprints, and also shows how to easily create new collisions
// (by adding "_" or "A" label with specific values, see below).
func TestFingerprintCollisions(t *testing.T) {
	// "8yn0iYCKYHlIj4-BwPqk" and "GReLUrM4wMqfg9yzV3KQ" have same FNV-1a hash.
	// If we use it as a single label name (for labels that have same value), we get colliding labels.
	c1 := labels.FromStrings("8yn0iYCKYHlIj4-BwPqk", "hello")
	c2 := labels.FromStrings("GReLUrM4wMqfg9yzV3KQ", "hello")
	verifyCollision(t, true, c1, c2)

	// Adding _="ypfajYg2lsv" or _="KiqbryhzUpn" respectively to most metrics will produce collision.
	// It's because "_\xffypfajYg2lsv" and "_\xffKiqbryhzUpn" have same FNV-1a hash, and "_" label is sorted before
	// most other labels (except labels starting with upper-case letter)

	const _label1 = "ypfajYg2lsv"
	const _label2 = "KiqbryhzUpn"

	metric := labels.NewBuilder(labels.FromStrings("__name__", "logs"))
	c1 = metric.Set("_", _label1).Labels()
	c2 = metric.Set("_", _label2).Labels()
	verifyCollision(t, true, c1, c2)

	metric = labels.NewBuilder(labels.FromStrings("__name__", "up", "instance", "hello"))
	c1 = metric.Set("_", _label1).Labels()
	c2 = metric.Set("_", _label2).Labels()
	verifyCollision(t, true, c1, c2)

	// here it breaks, because "Z" label is sorted before "_" label.
	metric = labels.NewBuilder(labels.FromStrings("__name__", "up", "Z", "hello"))
	c1 = metric.Set("_", _label1).Labels()
	c2 = metric.Set("_", _label2).Labels()
	verifyCollision(t, false, c1, c2)

	// A="K6sjsNNczPl" and A="cswpLMIZpwt" label has similar property.
	// (Again, because "A\xffK6sjsNNczPl" and "A\xffcswpLMIZpwt" have same FNV-1a hash)
	// This time, "A" is the smallest possible label name, and is always sorted first.

	const Alabel1 = "K6sjsNNczPl"
	const Alabel2 = "cswpLMIZpwt"

	metric = labels.NewBuilder(labels.FromStrings("__name__", "up", "Z", "hello"))
	c1 = metric.Set("A", Alabel1).Labels()
	c2 = metric.Set("A", Alabel2).Labels()
	verifyCollision(t, true, c1, c2)

	// Adding the same suffix to the "A" label also works.
	metric = labels.NewBuilder(labels.FromStrings("__name__", "up", "Z", "hello"))
	c1 = metric.Set("A", Alabel1+"suffix").Labels()
	c2 = metric.Set("A", Alabel2+"suffix").Labels()
	verifyCollision(t, true, c1, c2)
}

func verifyCollision(t *testing.T, collision bool, ls1 labels.Labels, ls2 labels.Labels) {
	if collision && Fingerprint(ls1) != Fingerprint(ls2) {
		t.Errorf("expected same fingerprints for %v (%016x) and %v (%016x)", ls1.String(), Fingerprint(ls1), ls2.String(), Fingerprint(ls2))
	} else if !collision && Fingerprint(ls1) == Fingerprint(ls2) {
		t.Errorf("expected different fingerprints for %v (%016x) and %v (%016x)", ls1.String(), Fingerprint(ls1), ls2.String(), Fingerprint(ls2))
	}
}
