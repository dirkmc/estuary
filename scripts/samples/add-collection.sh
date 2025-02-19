#!/bin/bash

###################################################################
#Script Name	: add-collection.sh                                                                                             
#Description	: This is a script file that runs a curl command to add a file to the estuary content server.                                                                                       
#Author         : ARG
#Email          : 
###################################################################

. run.config

fname=$(basename $EST_SAMPLE_FILE)
name="Sample Collection"
description="This is a sample collection"

# Let's add a collection
data="$(echo {} | jq --raw-output \
  --arg name "$name" \
  --arg description "$description" \
  '. + { "name": $name,
         "description": $description
       }'
)"

echo $data

set -x
curl --progress-bar -X POST -H "Authorization: Bearer $ESTUARY_TOKEN" -H "Content-Type: application/json" -d "$data" $EST_HOST/content/create
