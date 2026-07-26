package agentv1

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestModelProfileSnapshotChecksumsAndOptionalAgentSource(t *testing.T) {
	checksums := map[string]string{
		"core_model.proto":      "5be70269a71497c6738600517e193345f447d9fb03643e1bbcde42bbacadf1b6",
		"core_model.pb.go":      "120c0d4505436be79045c91d0976012c1a5f8fef9d944c76c3e526afcc3a90c2",
		"core_model_grpc.pb.go": "cbbe54c6046ebc92f54f5948f6fdf62416290376aff9bc594a7c993d5190f1c3",
	}
	for name, want := range checksums {
		got := fileSHA256(t, name)
		if got != want {
			t.Fatalf("%s checksum = %s, want %s", name, got, want)
		}
	}

	// CI may not mount the sibling Agent worktree. When it is available, compare
	// the copied snapshot byte-for-byte so regeneration/copy drift is visible.
	agentRoot := os.Getenv("DIREXTALK_AGENT_WORKTREE")
	if agentRoot == "" {
		cwd, err := os.Getwd()
		if err == nil {
			agentRoot = filepath.Join(cwd, "..", "..", "..", "..", "agent")
		}
	}
	sourcePaths := map[string]string{
		"core_model.proto":      filepath.Join("api", "proto", "dirextalk", "agent", "v1", "core_model.proto"),
		"core_model.pb.go":      filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_model.pb.go"),
		"core_model_grpc.pb.go": filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_model_grpc.pb.go"),
	}
	for snapshot, relative := range sourcePaths {
		source := filepath.Join(agentRoot, relative)
		_, statErr := os.Stat(source)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			t.Fatalf("stat optional Agent source %s: %v", source, statErr)
		}
		if got, want := fileSHA256(t, snapshot), fileSHA256Path(t, source); got != want {
			t.Fatalf("snapshot %s checksum %s differs from Agent source %s checksum %s", snapshot, got, source, want)
		}
	}
}

func TestConversationSnapshotChecksumsAndOptionalAgentSource(t *testing.T) {
	checksums := map[string]string{
		"core_conversation.proto":      "a7894cc03356e62fc1e35025cf50a3a944568bf7f5e9ea23f595ca6f48b08e11",
		"core_conversation.pb.go":      "fb7fd3302a246ed8d632ba53abe368acbea61fd48de8295212ef2478d989f1ae",
		"core_conversation_grpc.pb.go": "410ef140a890e06943512cafc5df94e6a42d61cb5bcde26933c8b2bcbe003418",
	}
	for name, want := range checksums {
		if got := fileSHA256(t, name); got != want {
			t.Fatalf("%s checksum = %s, want %s", name, got, want)
		}
	}
	agentRoot := os.Getenv("DIREXTALK_AGENT_WORKTREE")
	if agentRoot == "" {
		if cwd, err := os.Getwd(); err == nil {
			agentRoot = filepath.Join(cwd, "..", "..", "..", "..", "agent")
		}
	}
	sources := map[string]string{
		"core_conversation.proto":      filepath.Join("api", "proto", "dirextalk", "agent", "v1", "core_conversation.proto"),
		"core_conversation.pb.go":      filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_conversation.pb.go"),
		"core_conversation_grpc.pb.go": filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_conversation_grpc.pb.go"),
	}
	for snapshot, relative := range sources {
		source := filepath.Join(agentRoot, relative)
		_, err := os.Stat(source)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("stat optional Agent source %s: %v", source, err)
		}
		if got, want := fileSHA256(t, snapshot), fileSHA256Path(t, source); got != want {
			t.Fatalf("snapshot %s checksum %s differs from Agent source %s checksum %s", snapshot, got, source, want)
		}
	}
}

func TestExistingControlSnapshotChecksumsAndOptionalAgentSource(t *testing.T) {
	checksums := map[string]string{
		"core_task.proto":              "cb1cd42ab5aeb1cc229671aa1b524c351418053f752629fd7d7a1344861fa434",
		"core_task.pb.go":              "557e753cdb431f914f82875764b11e28e3ca7394e8d6aa81e387e13e80bb48de",
		"core_task_grpc.pb.go":         "c9946050dd5975cc5a1a673d7fea54d521b9a00c93566b15938dc190f8888bf6",
		"core_extension.proto":         "ed231ed784c2a3f6676a0bbb9920fc2a8f29c9b2426c213726f525755a34a483",
		"core_extension.pb.go":         "b4052eff92afb2b4d3a3adb8ba1ec78ab64375dc6fd77886d932922fde659d10",
		"core_extension_grpc.pb.go":    "034ecf64aa7019745a899f963de5d925f18286b98922c7f2de16ca00ecf40847",
		"core_schedule.proto":          "f8ebd4326d16e10e0869d747bf8b0978b0dc8ece9467f61f0a3c07ef40291de9",
		"core_schedule.pb.go":          "be498b2304cf8848a50cd747aa1684f6b6b80f86d7451fd6983082c44d2ecd1e",
		"core_schedule_grpc.pb.go":     "e12d5080f302e8b483e95985c7a2de52b72d7a1ea87464e1fb6682467cd48cd0",
		"core_confirmation.proto":      "190df5544a9c5eb36c89f9c93472e15dae15129452d4986abe7944ca96f4cd2e",
		"core_confirmation.pb.go":      "7752560233cd408798f7b85d8c66113eade22b251cbf44d1e3f6166c2854a904",
		"core_confirmation_grpc.pb.go": "81cc04cc452c52d79e3edb8ac1ec4b699b921124dde92dea6aa72ddbcc9b2024",
	}
	for name, want := range checksums {
		if got := fileSHA256(t, name); got != want {
			t.Fatalf("%s checksum = %s, want %s", name, got, want)
		}
	}
	agentRoot := os.Getenv("DIREXTALK_AGENT_WORKTREE")
	if agentRoot == "" {
		if cwd, err := os.Getwd(); err == nil {
			agentRoot = filepath.Join(cwd, "..", "..", "..", "..", "agent")
		}
	}
	sources := map[string]string{
		"core_task.proto":              filepath.Join("api", "proto", "dirextalk", "agent", "v1", "core_task.proto"),
		"core_task.pb.go":              filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_task.pb.go"),
		"core_task_grpc.pb.go":         filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_task_grpc.pb.go"),
		"core_extension.proto":         filepath.Join("api", "proto", "dirextalk", "agent", "v1", "core_extension.proto"),
		"core_extension.pb.go":         filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_extension.pb.go"),
		"core_extension_grpc.pb.go":    filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_extension_grpc.pb.go"),
		"core_schedule.proto":          filepath.Join("api", "proto", "dirextalk", "agent", "v1", "core_schedule.proto"),
		"core_schedule.pb.go":          filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_schedule.pb.go"),
		"core_schedule_grpc.pb.go":     filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_schedule_grpc.pb.go"),
		"core_confirmation.proto":      filepath.Join("api", "proto", "dirextalk", "agent", "v1", "core_confirmation.proto"),
		"core_confirmation.pb.go":      filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_confirmation.pb.go"),
		"core_confirmation_grpc.pb.go": filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_confirmation_grpc.pb.go"),
	}
	for snapshot, relative := range sources {
		source := filepath.Join(agentRoot, relative)
		_, statErr := os.Stat(source)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			t.Fatalf("stat optional Agent source %s: %v", source, statErr)
		}
		if got, want := fileSHA256(t, snapshot), fileSHA256Path(t, source); got != want {
			t.Fatalf("snapshot %s checksum %s differs from Agent source %s checksum %s", snapshot, got, source, want)
		}
	}
}

func TestWorkloadAWSSnapshotChecksumsAndOptionalAgentSource(t *testing.T) {
	checksums := map[string]string{
		"core_workload.proto":      "d36a42acd4e1410098c47853bd80eefe6f8d7cb737c920b23a31be38d5dd21e2",
		"core_workload.pb.go":      "5bc18bea484ea731c7a74acf8fa413e76f3f88bab2f880b53f3dee7b69f1f3eb",
		"core_workload_grpc.pb.go": "b2c219aaeb810a63b44d3fbdde641deed7cbf4d36bb361f3e0fd943fdf21a87e",
		"core_aws.proto":           "0b26e6ea760401ee91e79c2140f30fde59865e865d6974d642d01f03105fba5a",
		"core_aws.pb.go":           "2e394c7f157c586f0fcfc3068ccd685d992fd70809de2128ceea8416e663ff2a",
		"core_aws_grpc.pb.go":      "c9e2e7bfbe39d6a9c27abff72a5c8863c7c38821974ca972e27ede405ca86ef9",
	}
	for name, want := range checksums {
		if got := fileSHA256(t, name); got != want {
			t.Fatalf("%s checksum = %s, want %s", name, got, want)
		}
	}
	agentRoot := os.Getenv("DIREXTALK_AGENT_WORKTREE")
	if agentRoot == "" {
		if cwd, err := os.Getwd(); err == nil {
			agentRoot = filepath.Join(cwd, "..", "..", "..", "..", "agent")
		}
	}
	sources := map[string]string{"core_workload.proto": filepath.Join("api", "proto", "dirextalk", "agent", "v1", "core_workload.proto"), "core_workload.pb.go": filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_workload.pb.go"), "core_workload_grpc.pb.go": filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_workload_grpc.pb.go"), "core_aws.proto": filepath.Join("api", "proto", "dirextalk", "agent", "v1", "core_aws.proto"), "core_aws.pb.go": filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_aws.pb.go"), "core_aws_grpc.pb.go": filepath.Join("api", "gen", "dirextalk", "agent", "v1", "core_aws_grpc.pb.go")}
	for snapshot, relative := range sources {
		source := filepath.Join(agentRoot, relative)
		if _, err := os.Stat(source); os.IsNotExist(err) {
			continue
		}
		if got, want := fileSHA256(t, snapshot), fileSHA256Path(t, source); got != want {
			t.Fatalf("snapshot %s differs from Agent source", snapshot)
		}
	}
}

func fileSHA256(t *testing.T, name string) string {
	t.Helper()
	return fileSHA256Path(t, name)
}

func fileSHA256Path(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}
