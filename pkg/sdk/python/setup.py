# Copyright 2024 SandrPod
# Setup

from setuptools import setup, find_packages

setup(
    name="sandrpod-cli",
    version="0.2.0",
    packages=find_packages(),
    install_requires=[
        "requests>=2.28.0",
        "click>=8.0.0",
        "pyyaml>=6.0",
    ],
    entry_points={
        "console_scripts": [
            "sandrpod-cli=cli.main:cli",
        ],
    },
    package_data={
        "cli": ["py.typed"],
    },
)