package deploy

import "testing"

func TestObjectName(t *testing.T) {
	cases := map[[2]string]string{
		{"", "web"}:        "default/web",
		{"default", "web"}: "default/web",
		{"DEFAULT", "web"}: "default/web",
		{" noi ", " web "}: "noi/web",
	}
	for in, want := range cases {
		if got := ObjectName(in[0], in[1]); got != want {
			t.Errorf("ObjectName(%q,%q)=%q want %q", in[0], in[1], got, want)
		}
	}

	c := &Config{Context: "noi", Name: "web"}
	if c.Object() != "noi/web" {
		t.Fatal(c.Object())
	}
}
