from e2b_code_interpreter import Sandbox      # the official package, unmodified

sbx = Sandbox.create()
print("sandbox:", sbx.sandbox_id)

sbx.run_code("x = 21")                        # state survives between calls
print("stateful:", sbx.run_code("print(x * 2)").logs.stdout[0].strip())

r = sbx.run_code(
    "import matplotlib; matplotlib.use('Agg')\n"
    "import matplotlib.pyplot as plt; plt.plot([1,2,3],[2,4,9]); plt.show()"
)
print("chart:", len(r.results[0].png), "bytes of png")

sbx.kill()
print("killed")
