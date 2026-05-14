import json
from mpm_tool_schemas import MPM_TOOL_SCHEMAS, MPM_SYSTEM_PROMPT_DIRECTIVES
from mpm_tools import execute_mpm_query, execute_mpm_save

# 1. Inject the directives into OpenClaw's master system prompt
system_prompt += "\n" + MPM_SYSTEM_PROMPT_DIRECTIVES

# ... inside your main agent loop ...

# 2. Pass the schemas to the LLM
response = client.chat.completions.create(
    model="gpt-4o", # Or Claude/MiniMax
    messages=conversation_history,
    tools=MPM_TOOL_SCHEMAS,
    tool_choice="auto"
)

message = response.choices[0].message
conversation_history.append(message)

# 3. The Execution Router
if message.tool_calls:
    for tool_call in message.tool_calls:
        tool_name = tool_call.function.name
        tool_args = json.loads(tool_call.function.arguments)

        print(f"🧠 [MPM Synapse] Agent invoked: {tool_name}")

        # Route to the correct Go subprocess wrapper
        if tool_name == "query_long_term_memory":
            result = execute_mpm_query(**tool_args)
            # Convert the dataclass result to a JSON string for the LLM
            tool_response = json.dumps(result.__dict__)

        elif tool_name == "save_to_memory":
            result = execute_mpm_save(**tool_args)
            tool_response = json.dumps(result.__dict__)
            
        else:
            tool_response = json.dumps({"error": f"Unknown tool: {tool_name}"})

        # 4. Feed the SQLite result back to the LLM
        conversation_history.append({
            "role": "tool",
            "tool_call_id": tool_call.id,
            "name": tool_name,
            "content": tool_response
        })

    # The agent now has the memories in its context! 
    # Usually, you loop back and let the LLM generate its final text response here.