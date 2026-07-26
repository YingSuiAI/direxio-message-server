# Agent Core protobuf snapshot

The discovery files (`core.pb.go`, `core_grpc.pb.go`) are snapshots from the
Agent baseline commit `11eed51e2a9e6431f28039a542f2424f290e6fff`.

They are kept local because the Agent repository is a separate Go module and is
not an importable dependency of Message Server. The source generated-file
checksums are:

* `core.pb.go`: `09005933682d94dec952aada7746f3d15608621459de982b6638b335b8e703c2`
* `core_grpc.pb.go`: `0fd9b87400c006603890da0e37f8fb0fd648976413d006c3719da3a35ac829eb`

The model-profile files are copied from the uncommitted integration worktree
`/home/adam/dirextalk/worktrees/agent-core-v1-integration/agent`, branch
`adam/agent-core-v1-integration`, whose current HEAD is still the baseline
commit above. They include the atomic `Sync` RPC and write-only API-key
presence field. They must not be described as baseline-generated files until
the paired Agent changes are committed.

Source content and baseline-diff provenance:

* source `core_model.proto`: `5be70269a71497c6738600517e193345f447d9fb03643e1bbcde42bbacadf1b6`
* source diff from baseline: `024539f055474b4d4a07e3a37f6c4380ccef001f746adf04048cecb5bcac667b`

* `core_model.pb.go`: `120c0d4505436be79045c91d0976012c1a5f8fef9d944c76c3e526afcc3a90c2`
* generated diff from baseline: `af1241e87d7425254d3c630a8edcfad804e85e4c20791b6c2ab739fa94630d76`
* `core_model_grpc.pb.go`: `cbbe54c6046ebc92f54f5948f6fdf62416290376aff9bc594a7c993d5190f1c3`
* generated gRPC diff from baseline: `8f6431c1100678d73e382cffba602dbca9315771edd78c1e2a2943807bdc13c2`

`core_model.proto` is included beside the generated Go files as a provenance
snapshot; it is not compiled by the Message Server module.

To reproduce the source checksums, run `sha256sum` on the three source files in
the Agent worktree. To reproduce each baseline-diff checksum, run
`git -C /home/adam/dirextalk/worktrees/agent-core-v1-integration/agent diff
11eed51e2a9e6431f28039a542f2424f290e6fff -- <path> | sha256sum`.

Only `AgentService.GetCapabilities` and `AgentService.GetInstanceInfo` plus
`ModelProfileService` model-profile RPCs are used by the bounded adapters.
Runtime/service-key patterns are not part of this snapshot; API keys are never
returned in model-profile projections.

The durable conversation adapter additionally snapshots
`core_conversation.proto`, `core_conversation.pb.go`, and
`core_conversation_grpc.pb.go` from the same Agent worktree. Their current
source checksums are recorded in `provenance_test.go`; the generated
`core_extension` and `core_task` files are included only as descriptor
dependencies required by the conversation protobuf and are not independently
exposed by Message Server.

Current conversation snapshot checksums: proto
`a7894cc03356e62fc1e35025cf50a3a944568bf7f5e9ea23f595ca6f48b08e11`, generated
Go `fb7fd3302a246ed8d632ba53abe368acbea61fd48de8295212ef2478d989f1ae`, and
gRPC `410ef140a890e06943512cafc5df94e6a42d61cb5bcde26933c8b2bcbe003418`.

The control-plane task/extension snapshots and the Schedule/Confirmation
snapshots are copied from the normalized Agent generation in the integration
worktree (buf `1.72.0`, protoc-gen-go `1.36.11`, protoc-gen-go-grpc `1.5.1`).
Their source/generated checksums are recorded here so a regeneration can be
audited without importing the Agent module:

* `core_task.proto`: `cb1cd42ab5aeb1cc229671aa1b524c351418053f752629fd7d7a1344861fa434`
* `core_task.pb.go`: `557e753cdb431f914f82875764b11e28e3ca7394e8d6aa81e387e13e80bb48de`
* `core_task_grpc.pb.go`: `c9946050dd5975cc5a1a673d7fea54d521b9a00c93566b15938dc190f8888bf6`
* `core_extension.proto`: `485ae631f7096e71a3830e76f10cf85f3c0a3f5d310193b88ed3c721f88522fe`
* `core_extension.pb.go`: `4bab907985ccadb99a48d7362a18030104d9775f64c0a5d31949b2d69aca6d2a`
* `core_extension_grpc.pb.go`: `034ecf64aa7019745a899f963de5d925f18286b98922c7f2de16ca00ecf40847`
* `core_schedule.proto`: `f8ebd4326d16e10e0869d747bf8b0978b0dc8ece9467f61f0a3c07ef40291de9` (source snapshot)
* `core_schedule.pb.go`: `be498b2304cf8848a50cd747aa1684f6b6b80f86d7451fd6983082c44d2ecd1e`
* `core_schedule_grpc.pb.go`: `e12d5080f302e8b483e95985c7a2de52b72d7a1ea87464e1fb6682467cd48cd0`
* `core_confirmation.proto`: `1e1f5222fe7ef1ebf2d2ef6f9c1ce650bab572c483fe5c7263bad3d060ae6a6e`
* `core_confirmation.pb.go`: `c1b50aebe54eef820e503dbf6ecad475575dbe6329c33a91c573a076c4e126ab`
* `core_confirmation_grpc.pb.go`: `6f1c3752506af39c9d7fad11072cb997f5a3815d5df542a86bce2f5d00a309f3`

`provenance_test.go` verifies these checksums and, when
`DIREXTALK_AGENT_WORKTREE` is mounted, compares every existing snapshot family
byte-for-byte with its optional Agent source.

Workload/AWS snapshots are copied from Agent commit
`1a966efde8fcb1042a2647e60eb86f74de3214b4`:

* `core_workload.proto`: `d36a42acd4e1410098c47853bd80eefe6f8d7cb737c920b23a31be38d5dd21e2`
* `core_workload.pb.go`: `5bc18bea484ea731c7a74acf8fa413e76f3f88bab2f880b53f3dee7b69f1f3eb`
* `core_workload_grpc.pb.go`: `b2c219aaeb810a63b44d3fbdde641deed7cbf4d36bb361f3e0fd943fdf21a87e`
* `core_aws.proto`: `0b26e6ea760401ee91e79c2140f30fde59865e865d6974d642d01f03105fba5a`
* `core_aws.pb.go`: `2e394c7f157c586f0fcfc3068ccd685d992fd70809de2128ceea8416e663ff2a`
* `core_aws_grpc.pb.go`: `c9e2e7bfbe39d6a9c27abff72a5c8863c7c38821974ca972e27ede405ca86ef9`
