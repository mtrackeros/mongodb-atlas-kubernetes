package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
)

const (
	googleProjectID   = "atlasoperator"
	gcpVPCName        = "network-peering-gcp-1-vpc"
	resourceGroupName = "svet-test"
	azureVPCName      = "test-vnet"
)

func main() {
	err := setGCPCredentials()
	if err != nil {
		log.Fatal(err)
	}
	var allErr error
	gcpOk, err := deleteGCPVPCBySubstr(googleProjectID, gcpVPCName)
	if err != nil {
		allErr = errors.Join(allErr, err)
		log.Println(err)
	}
	if !gcpOk {
		log.Println("Not all GCP VPC was deleted")
	}
	subID := os.Getenv("AZURE_SUBSCRIPTION_ID")
	if subID == "" {
		log.Fatal("AZURE_SUBSCRIPTION_ID is not set")
	}
	ctx := context.Background()
	azureOk, err := deleteAzureVPCBySubstr(ctx, subID, resourceGroupName, azureVPCName)
	if err != nil {
		allErr = errors.Join(allErr, err)
		log.Println(err)
	}
	if !azureOk {
		log.Println("Not all Azure VPC was deleted")
	}
	if allErr != nil {
		fmt.Println("ERRORS: ", allErr)
	}
	if !azureOk || !gcpOk {
		os.Exit(1)
	}
}
