class Berth < Formula
  desc "Dead simple PaaS for Mac minis: git push and your app is live"
  homepage "https://github.com/dps/berth"
  url "https://github.com/dps/berth/archive/refs/tags/v0.1.2.tar.gz"
  sha256 "1118f64daef80efb566d19d00b1140bfeb365cf89e51934baf31dc31e210ab79"
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
