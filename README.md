# ECR Cleanup

[![Release](https://img.shields.io/github/v/release/sersoft-gmbh/ecr-cleanup?display_name=tag&sort=semver)](https://github.com/sersoft-gmbh/ecr-cleanup/releases)
[![Build](https://github.com/sersoft-gmbh/ecr-cleanup/actions/workflows/build.yml/badge.svg)](https://github.com/sersoft-gmbh/ecr-cleanup/actions/workflows/build.yml)

Cleans Amazon ECR docker repositories by deleting unused images.
Whether an image is used is determined by inspecting a kubernetes cluster.
