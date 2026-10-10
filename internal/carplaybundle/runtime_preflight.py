"""检查动态 Python 模块闭包，不运行蓝牙、USB、网络或限时实验入口。"""
import ast
import importlib.util
from pathlib import Path
import sys


def verify(runtime):
    """所有本地依赖必须来自已验签公开版本，不能借用旧安装目录掩盖漏包。"""
    python = Path(runtime) / "python"
    seen = set()

    def visit(name):
        path = python / name
        if name in seen:
            return
        if not path.is_file() or path.is_symlink() or path.stat().st_size > 512000:
            raise ValueError("CarPlay 本地模块缺失或超出预算")
        seen.add(name)
        source = path.read_text(encoding="utf-8")
        tree = ast.parse(source, filename=name)
        for node in ast.walk(tree):
            if not isinstance(node, ast.Call):
                continue
            function = node.func
            if isinstance(function, ast.Name) and function.id == "load_module" and node.args:
                argument = node.args[-1]
                if isinstance(argument, ast.Constant) and isinstance(argument.value, str) and argument.value.endswith(".py"):
                    if Path(argument.value).name != argument.value:
                        raise ValueError("CarPlay 本地模块路径不合法")
                    visit(argument.value)
        # 就绪等待脚本有明确顶层等待，仅检查语法；其他模块的硬件入口都有 main 防护。
        if name != "carplay-runtime-ready.py":
            spec = importlib.util.spec_from_file_location("preflight_" + str(len(seen)), path)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)

    visit("carplay-service.py")
    visit("carplay-runtime-ready.py")
    return len(seen)


if __name__ == "__main__":
    verify(Path(sys.argv[1]))
