const providerPlugin = [
    "@semantic-release/github",
    {
        successComment: false,
        failComment: false,
        releasedLabels: false
    }
];

module.exports = {
    branches: ["main"],
    tagFormat: "v${version}",
    plugins: [
        [
            "@semantic-release/commit-analyzer",
            {
                preset: "conventionalcommits",
                parserOpts: {
                    noteKeywords: [
                        "BREAKING CHANGE",
                        "BREAKING CHANGES",
                        "BREAKING"
                    ]
                },
                releaseRules: [
                    {
                        type: "build",
                        release: false
                    },
                    {
                        type: "ci",
                        release: false
                    },
                    {
                        type: "docs",
                        release: false
                    },
                    {
                        type: "style",
                        release: false
                    },
                    {
                        type: "test",
                        release: false
                    },
                    {
                        type: "chore",
                        release: false
                    },
                    {
                        type: "refactor",
                        release: "patch"
                    },
                    {
                        type: "perf",
                        release: "patch"
                    }
                ]
            }
        ],
        [
            "@semantic-release/release-notes-generator",
            {
                preset: "conventionalcommits",
                parserOpts: {
                    noteKeywords: [
                        "BREAKING CHANGE",
                        "BREAKING CHANGES",
                        "BREAKING"
                    ]
                }
            }
        ],
        providerPlugin
    ]
};
