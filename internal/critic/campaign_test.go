package critic

import "testing"

func TestExampleCampaignsParse(t *testing.T) {
	for _, f := range []string{"refstack-nightly", "refstack-smoke"} {
		c, err := LoadCampaign("../../examples/campaigns/" + f + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		if c.Spec.Stack == nil || len(c.Spec.Scenarios.Files) == 0 {
			t.Fatalf("%s: %+v", f, c.Spec)
		}
	}
}

func TestCriteria(t *testing.T) {
	base := 100.0
	for _, tc := range []struct {
		c    string
		v    float64
		want bool
	}{{"<=45s", 45, true}, {"<=45s", 45.1, false}, {"==0", 0, true}, {">=0.9", 0.95, true}, {"<=+10%", 110, true}, {"<=+10%", 111, false}, {"<2m", 119, true}} {
		c, err := parseCriterion(tc.c)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.holds(tc.v, &base); got != tc.want {
			t.Errorf("%s holds for %g: %v, want %v", tc.c, tc.v, got, tc.want)
		}
	}
}
