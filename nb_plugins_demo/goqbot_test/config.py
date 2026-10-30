from pydantic import BaseModel


class Config(BaseModel):
    # 图片 URL 测试用的公网图片（QQ 平台需能直接拉取）
    goqbot_test_image_url: str = "https://q1.qlogo.cn/g?b=qq&nk=10001&s=640"
    # 视频 / 语音测试的默认来源：http(s) URL、file:// 或本地绝对路径（goqbot 进程须能读到）
    goqbot_test_video: str = ""
    goqbot_test_record: str = ""
    # 撤回测试：发送后等待多少秒再撤回
    goqbot_test_recall_delay: int = 5
    # 测试产生的临时文件目录（须 goqbot 与 NoneBot 在同一台机器上，本地路径类用例才有效）
    goqbot_test_tmp_dir: str = ""
    # 收到 friend_add / 机器人入群通知时是否自动回一条欢迎消息
    goqbot_test_notice_reply: bool = True
