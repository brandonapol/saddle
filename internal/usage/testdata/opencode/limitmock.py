# A stand-in for OpenCode Zen and OpenAI that answers every request with the
# exact error body, status and headers the real service sends for a limit.
import http.server, json, sys, os
MODE=os.environ.get("MODE","free"); RA=os.environ.get("RA")
LOG=open(os.environ["MOCKLOG"],"a")
def zen(t, msg, meta=None):
    return {"type":"error","error":{"type":t,"message":msg},"metadata":meta or {}}
CASES={
 # ipRateLimiter.ts: FreeUsageLimitError(dict["zen.api.error.rateLimitExceeded"], retryAfter=seconds to UTC midnight)
 "free": (429, zen("FreeUsageLimitError","Rate limit exceeded. Please try again later."), "34200"),
 # handler.ts: GoUsageLimitError(goSubscriptionRollingLimitExceeded, ws, "5 hour", resetInSec)
 "go": (429, zen("GoUsageLimitError","5-hour usage limit reached. Resets in 2hr 5min. To continue using this model now, enable usage from your available balance: https://opencode.ai/workspace/wrk_123/go",{"workspace":"wrk_123","limitName":"5 hour"}), "7500"),
 # handler.ts: CreditsError(insufficientBalance) -> 401
 "credits": (401, zen("CreditsError","Insufficient balance. Manage your billing here: https://opencode.ai/workspace/wrk_123/billing"), None),
 # handler.ts: MonthlyLimitError(workspaceMonthlyLimitReached) -> 401
 "monthly": (401, zen("MonthlyLimitError","Your workspace has reached its monthly spending limit of $20. Manage your limits here: https://opencode.ai/workspace/wrk_123/billing"), None),
 # OpenAI's documented insufficient_quota 429
 "openai": (429, {"error":{"message":"You exceeded your current quota, please check your plan and billing details. For more information on this error, read the docs: https://platform.openai.com/docs/guides/error-codes/api-errors.","type":"insufficient_quota","param":None,"code":"insufficient_quota"}}, None),
}
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n=int(self.headers.get('content-length') or 0); self.rfile.read(n)
        st,out,ra=CASES[MODE]
        if RA is not None: ra=RA or None
        LOG.write(f"{self.command} {self.path} {MODE} {st} ra={ra}\n"); LOG.flush()
        b=json.dumps(out).encode()
        self.send_response(st); self.send_header("content-type","application/json")
        if ra: self.send_header("retry-after",ra)
        self.send_header("content-length",str(len(b))); self.end_headers(); self.wfile.write(b)
    do_GET=do_POST
    def log_message(self,*a): pass
http.server.ThreadingHTTPServer(("127.0.0.1",int(sys.argv[1])),H).serve_forever()
