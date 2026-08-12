package main

import (
	"context"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/grumpy-penguin/terraform-provider-mssql/internal/provider"
)

// version is set via -ldflags at build time.
var version = "dev"

func main() {
	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/local/mssql",
	})
	if err != nil {
		log.Fatal(err)
	}
}
