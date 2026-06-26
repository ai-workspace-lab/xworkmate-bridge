package acp
import "testing"
func TestS1DefaultExpectedArtifactDirs(t *testing.T) {
	// 任务消息要求产出 md → 推断 requiredExts，但未声明 expectedArtifactDirs → 应补缺省目录
	c := openClawArtifactContractForParams(
		map[string]any{}, map[string]any{"message": "采集最新AI资讯，保存在md文件"})
	if len(c.ExpectedArtifactDirs) == 0 {
		t.Fatalf("expected default artifact dirs for an md-producing task, got none")
	}
	if !c.RequiresArtifactExport {
		t.Fatalf("expected RequiresArtifactExport=true when default dirs applied")
	}
	// 纯聊天（无产物意图）→ 不应补目录、不应强制导出
	c2 := openClawArtifactContractForParams(
		map[string]any{}, map[string]any{"message": "你好，介绍一下你自己"})
	if len(c2.ExpectedArtifactDirs) != 0 || c2.RequiresArtifactExport {
		t.Fatalf("pure chat should not get default dirs / forced export, got dirs=%v export=%v", c2.ExpectedArtifactDirs, c2.RequiresArtifactExport)
	}
}
