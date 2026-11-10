package main

import (
	// Embed the IANA timezone database so the scratch container (and minimal
	// hosts) honor TZ without a tzdata package.
	_ "time/tzdata"

	"github.com/6Kmfi6HP/opencode2api/internal/app"
)

func main() {
	app.Run()
}
