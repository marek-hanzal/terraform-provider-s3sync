package main

import (
	"context"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/marek-hanzal/terraform-provider-s3sync/internal/provider"
)

var version = "dev"

func main() {
	if err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/marek-hanzal/s3sync",
	}); err != nil {
		log.Fatal(err)
	}
}
