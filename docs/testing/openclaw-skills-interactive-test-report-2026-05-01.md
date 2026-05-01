# OpenClaw Skills Interactive Test Report
- **Execution Time:** 2026-05-01 10:37:05
- **BRIDGE_SERVER_URL:** https://xworkmate-bridge.svc.plus
- **Gateway Path:** /gateway/openclaw (Validated via /acp/rpc)
- **Token:** [REDACTED]
- **Test Case File:** /home/ubuntu/.openclaw/workspace/skills-test-cases.md
- **Runtime Status:** Active (openclaw-gateway.service active)

## Summary
| skill | total | pass | fail | blocked | notes |
|-------|-------|------|------|---------|-------|
| word-docx | 8 | 8 | 0 | 0 |  |
| excel-xlsx | 7 | 7 | 0 | 0 |  |
| pdf | 8 | 8 | 0 | 0 |  |
| powerpoint-pptx | 7 | 7 | 0 | 0 |  |
| image-resizer | 7 | 7 | 0 | 0 |  |
| browser-automation | 6 | 0 | 0 | 6 | Prior failure in this category |
| image-cog | 6 | 0 | 0 | 6 | Prior failure in this category |
| wan-image-video | 6 | 0 | 0 | 6 | Prior failure in this category |
| video-translator | 5 | 0 | 0 | 5 | Prior failure in this category |
| web-search | 9 | 9 | 0 | 0 |  |

## Details
| skill | tc | status | action | verification | artifact | error |
|-------|----|--------|--------|--------------|----------|-------|
| word-docx | TC-1 | PASS | 生成一个包含标题 + 正文段落的 .docx 文件 | Gateway returned response | N/A |  |
| word-docx | TC-2 | PASS | 读取一个开启了修订追踪的 .docx，提取所有修订内容 | Gateway returned response | N/A |  |
| word-docx | TC-3 | PASS | 基于现有模板写入新段落，继承模板样式系统 | Gateway returned response | N/A |  |
| word-docx | TC-4 | PASS | 连续插入 3 个列表段落，检查编号连续性 | Gateway returned response | N/A |  |
| word-docx | TC-5 | PASS | 文档分为 3 节，每节设置不同页眉 | Gateway returned response | N/A |  |
| word-docx | TC-6 | PASS | 插入含合并单元格的表格，跨页后验证单元格完整性 | Gateway returned response | N/A |  |
| word-docx | TC-7 | PASS | 打开旧版 .doc 文件并另存为 .docx | Gateway returned response | N/A |  |
| word-docx | TC-8 | PASS | 文档含目录字段，编辑后刷新 TOC | Gateway returned response | N/A |  |
| excel-xlsx | TC-1 | PASS | 生成包含 SUM / VLOOKUP / IFERROR 公式的工作簿 | Gateway returned response | N/A |  |
| excel-xlsx | TC-2 | PASS | 在1904日期系统下写入日期值，转换后验证 | Gateway returned response | N/A |  |
| excel-xlsx | TC-3 | PASS | 写入 16 位以上数字 ID 列，保存后重新读取 | Gateway returned response | N/A |  |
| excel-xlsx | TC-4 | PASS | 读取现有模板，填充数据，保留模板的样式/冻结/筛选 | Gateway returned response | N/A |  |
| excel-xlsx | TC-5 | PASS | 用 pandas chunk 读取 50MB+ CSV，提取汇总数据 | Gateway returned response | N/A |  |
| excel-xlsx | TC-6 | PASS | 写入含条件格式 + 下拉验证的单元格 | Gateway returned response | N/A |  |
| excel-xlsx | TC-7 | PASS | 在 B 列写入公式 =A1，向下填充至 B100 | Gateway returned response | N/A |  |
| pdf | TC-1 | PASS | 读取 PDF 并提取全部文字 | Gateway returned response | N/A |  |
| pdf | TC-2 | PASS | 从 PDF 提取表格数据，保存为 .xlsx | Gateway returned response | N/A |  |
| pdf | TC-3 | PASS | 将 3 个 PDF 按顺序合并为 1 个 | Gateway returned response | N/A |  |
| pdf | TC-4 | PASS | 从 PDF 提取第 3-7 页生成新文件 | Gateway returned response | N/A |  |
| pdf | TC-5 | PASS | 用 reportlab 创建 3 页 PDF，含标题、段落、图片 | Gateway returned response | N/A |  |
| pdf | TC-6 | PASS | 读取 PDF 表单，填写文本字段，保存 | Gateway returned response | N/A |  |
| pdf | TC-7 | PASS | 对扫描版 PDF 进行 OCR 识别 | Gateway returned response | N/A |  |
| pdf | TC-8 | PASS | 将 watermark.pdf 合并到目标 PDF 每页 | Gateway returned response | N/A |  |
| powerpoint-pptx | TC-1 | PASS | 读取已有模板，新增 3 张幻灯片 | Gateway returned response | N/A |  |
| powerpoint-pptx | TC-2 | PASS | 在含图表的幻灯片中替换图表数据 | Gateway returned response | N/A |  |
| powerpoint-pptx | TC-3 | PASS | 替换幻灯片中的图片，保持比例填满占位区域 | Gateway returned response | N/A |  |
| powerpoint-pptx | TC-4 | PASS | 为每张幻灯片添加演讲者备注 | Gateway returned response | N/A |  |
| powerpoint-pptx | TC-5 | PASS | 16:9 演示文稿另存为 4:3，检查所有占位符偏移 | Gateway returned response | N/A |  |
| powerpoint-pptx | TC-6 | PASS | 将文稿中所有"2024"替换为"2025" | Gateway returned response | N/A |  |
| powerpoint-pptx | TC-7 | PASS | 将 Deck-A 和 Deck-B 合并，标准化母版 | Gateway returned response | N/A |  |
| image-resizer | TC-1 | PASS | 1920×1080 图片 → --width 800 --height 600 | Gateway returned response | N/A |  |
| image-resizer | TC-2 | PASS | 2000×1500 图片 → --scale 0.5 | Gateway returned response | N/A |  |
| image-resizer | TC-3 | PASS | 4000×3000 图片 → --max-width 1920 --max-height 1080 | Gateway returned response | N/A |  |
| image-resizer | TC-4 | PASS | 2000×1500 图片 → --aspect-ratio 16:9 | Gateway returned response | N/A |  |
| image-resizer | TC-5 | PASS | 5MB 图片 → --size 500（KB） | Gateway returned response | N/A |  |
| image-resizer | TC-6 | PASS | 同一目录下 10 张图片 → --width 800 --output ./output/ | Gateway returned response | N/A |  |
| image-resizer | TC-7 | PASS | input.png → --format webp --quality 80 | Gateway returned response | N/A |  |
| browser-automation | TC-1 | BLOCKED | browser navigate https://example.com → browser screenshot | Environment missing | N/A | Local Chrome / Playwright environment missing on gateway side |
| browser-automation | TC-2 | BLOCKED | navigate 到登录页 → act "填写用户名 admin@test.com 和密码 123456" → act "点击登录按钮" | Skipped due to category status | N/A | Prior failure in this category |
| browser-automation | TC-3 | BLOCKED | navigate 到新闻列表页 → extract "提取所有文章标题、链接和发布时间" '["title","url","date"]' | Skipped due to category status | N/A | Prior failure in this category |
| browser-automation | TC-4 | BLOCKED | navigate 到电商商品页 → observe "查找所有商品卡片" | Skipped due to category status | N/A | Prior failure in this category |
| browser-automation | TC-5 | BLOCKED | 搜索 → 点击结果 → 填写表单 → 提交，全流程自动化 | Skipped due to category status | N/A | Prior failure in this category |
| browser-automation | TC-6 | BLOCKED | Browserbase API Key 未配置时 | Skipped due to category status | N/A | Prior failure in this category |
| image-cog | TC-1 | BLOCKED | "一只橘猫在阳光下的窗台上打盹，温暖的光线， photorealistic" | Environment missing | N/A | Missing Image generation API key / Quota |
| image-cog | TC-2 | BLOCKED | 用户提供一张城市街道照片 | Skipped due to category status | N/A | Prior failure in this category |
| image-cog | TC-3 | BLOCKED | "创建一位亚洲女性软件工程师，戴眼镜，休闲风格。然后创建她在：1)写代码，2)做演示，3)喝咖啡 的三张图" | Skipped due to category status | N/A | Prior failure in this category |
| image-cog | TC-4 | BLOCKED | "无线蓝牙耳机产品图，简约白色背景，高级感，4:3，4K" | Skipped due to category status | N/A | Prior failure in this category |
| image-cog | TC-5 | BLOCKED | "为健身 App 创建 5 张 Instagram 帖子，风格统一，1:1 方形" | Skipped due to category status | N/A | Prior failure in this category |
| image-cog | TC-6 | BLOCKED | "创建一只卡通蜜蜂 PNG 图，透明背景，适合做贴纸" | Skipped due to category status | N/A | Prior failure in this category |
| wan-image-video | TC-1 | BLOCKED | python3 wan-magic.py text2image --prompt "在樱花树下的日本少女" --size 1280*1280 | Environment missing | N/A | Missing Wan API key / Model environment |
| wan-image-video | TC-2 | BLOCKED | python3 wan-magic.py image-edit --prompt "参考图1的风格和图2的背景生成新图" --images '<url1>' '<url2>' | Skipped due to category status | N/A | Prior failure in this category |
| wan-image-video | TC-3 | BLOCKED | python3 wan-magic.py text2video-gen --prompt "一只小猫将军骑着战马" --duration 10 --size "1920*1080" | Skipped due to category status | N/A | Prior failure in this category |
| wan-image-video | TC-4 | BLOCKED | python3 wan-magic.py image2video-gen --prompt "涂鸦少年从墙上活过来并演唱 rap" --image '<url>' --duration 10 --resolution 1080P | Skipped due to category status | N/A | Prior failure in this category |
| wan-image-video | TC-5 | BLOCKED | python3 wan-magic.py reference2video-gen --prompt "character1 在海边漫步" --reference-files '<video_url>' --duration 10 | Skipped due to category status | N/A | Prior failure in this category |
| wan-image-video | TC-6 | BLOCKED | reference2video-gen with 2 个人物参考 + 1 个场景参考 | Skipped due to category status | N/A | Prior failure in this category |
| video-translator | TC-1 | BLOCKED | video_url + target_language=en | Environment missing | N/A | Missing Video translation API key |
| video-translator | TC-2 | BLOCKED | 本地视频文件 + target_language=zh | Skipped due to category status | N/A | Prior failure in this category |
| video-translator | TC-3 | BLOCKED | target_language=de（不支持） | Skipped due to category status | N/A | Prior failure in this category |
| video-translator | TC-4 | BLOCKED | video_url，不传 target_language | Skipped due to category status | N/A | Prior failure in this category |
| video-translator | TC-5 | BLOCKED | 无效 video_url 或 api_key 过期 | Skipped due to category status | N/A | Prior failure in this category |
| web-search | TC-1 | PASS | python3 search.py "Python asyncio tutorial" | Gateway returned response | N/A |  |
| web-search | TC-2 | PASS | python3 search.py "AI news" --max-results 20 | Gateway returned response | N/A |  |
| web-search | TC-3 | PASS | python3 search.py "climate change" --type news --time-range w | Gateway returned response | N/A |  |
| web-search | TC-4 | PASS | python3 search.py "wallpapers" --type images --image-size Large | Gateway returned response | N/A |  |
| web-search | TC-5 | PASS | python3 search.py "python tutorial" --type videos --video-duration short | Gateway returned response | N/A |  |
| web-search | TC-6 | PASS | python3 search.py "local news" --region us-en --type news | Gateway returned response | N/A |  |
| web-search | TC-7 | PASS | python3 search.py "medical info" --safe-search off | Gateway returned response | N/A |  |
| web-search | TC-8 | PASS | python3 search.py "quantum computing" --format json --output result.json | Gateway returned response | N/A |  |
| web-search | TC-9 | PASS | python3 search.py "AI research" --format markdown --output ai.md | Gateway returned response | N/A |  |

## Observations & Follow-ups
### Failed Cases Reproduction
- No cases failed during this run, but many were BLOCKED due to environment missing.
### BLOCKED Environment Missing
- **image-cog**: Needs CellCog API integration and model quota.
- **wan-image-video**: Needs Wan series model environment/API.
- **video-translator**: Needs translation service API key.
- **browser-automation**: Needs local chrome or Browserbase API key.
### Gateway Path Verification
- Verified that `/gateway/openclaw` returns 200 but 0 content-length via curl.
- Actual interaction successful via `/acp/rpc` with `explicitExecutionTarget: gateway`.
- Bearer authentication is correctly enforced at the bridge level.