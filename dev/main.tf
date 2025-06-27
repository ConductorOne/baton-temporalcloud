terraform {
  required_providers {
    temporalcloud = {
      source = "temporalio/temporalcloud"
    }
  }
}

provider "temporalcloud" {
  allowed_account_id = "iv3js"
}

resource "temporalcloud_namespace" "test_namespace" {
  name = var.TEST_NAMESPACE_NAME,
  regions = ["aws-us-west-2"]
  api_key_auth = true
  retention_days = 7
}