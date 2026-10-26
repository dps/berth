class Berth < Formula
  desc "Dead simple PaaS for Mac minis: git push and your app is live"
  homepage "https://github.com/dps/berth"
  url "https://github.com/dps/berth/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "a92a8c52d653c23af5adabe1ddafd902197b9da2f3820c2c99558fada2eeab2d"
  license "MIT"
  head "https://github.com/dps/berth.git", branch: "main"

  depends_on "go" => :build
  depends_on arch: :arm64
  depends_on macos: :tahoe # macOS 26: Apple's container needs it
  depends_on "caddy"
  depends_on "cloudflared"
  depends_on "container"

  def install
    ldflags = "-s -w -X main.version=#{version}"
    system "go", "build", *std_go_args(ldflags:, output: bin/"berth"), "./cmd/berth"
    system "go", "build", *std_go_args(ldflags:, output: bin/"berthd"), "./cmd/berthd"
    generate_completions_from_executable(bin/"berth", "completion")
  end

  def caveats
    <<~EOS
      Set up once (about three minutes):
        berth init --domain example.com
      Then deploy an app:
        berth create myapp --repo you/myapp
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/berthd --version")
    assert_match "berth is not initialized", shell_output("BERTH_HOME=#{testpath}/b #{bin}/berth status 2>&1", 1)
  end
end
