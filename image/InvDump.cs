using System;
using System.Collections.Generic;
using System.Text;
using Oxide.Core;

namespace Oxide.Plugins
{
    [Info("InvDump", "ai-box", "2.0.0")]
    [Description("On-demand player inventory JSON for the web panel: invdump.get <steamid>")]
    public class InvDump : RustPlugin
    {
        // Invoked over RCON/console by the control panel; replies with
        // {"player":{...}} or null. No timers, no files: zero cost unless asked.
        [ConsoleCommand("invdump.get")]
        private void CmdGet(ConsoleSystem.Arg arg)
        {
            if (!arg.IsAdmin) return;
            if (arg.Args == null || arg.Args.Length < 1)
            {
                arg.ReplyWith("usage: invdump.get <steamid>");
                return;
            }
            string sid = arg.Args[0].ToString();
            BasePlayer target = null;
            foreach (var p in BasePlayer.activePlayerList)
            {
                if (p != null && p.IsConnected && p.UserIDString == sid) { target = p; break; }
            }
            var sb = new StringBuilder(4096);
            if (target == null)
            {
                sb.Append("null");
            }
            else
            {
                sb.Append("{\"player\":");
                AppendPlayer(sb, target);
                sb.Append('}');
            }
            arg.ReplyWith(sb.ToString());
        }

        private void AppendPlayer(StringBuilder sb, BasePlayer p)
        {
            sb.Append('{');
            sb.Append("\"name\":").Append(JStr(p.displayName));
            if (p.metabolism != null)
            {
                sb.Append(",\"calories\":").Append((int)p.metabolism.calories.value);
                sb.Append(",\"hydration\":").Append((int)p.metabolism.hydration.value);
            }
            sb.Append(",\"belt\":");   AppendContainer(sb, p.inventory.containerBelt, 0);
            sb.Append(",\"wear\":");   AppendContainer(sb, p.inventory.containerWear, 0);
            sb.Append(",\"main\":");   AppendContainer(sb, p.inventory.containerMain, 0);
            sb.Append('}');
        }

        private void AppendContainer(StringBuilder sb, ItemContainer c, int depth)
        {
            sb.Append('[');
            if (c != null && c.itemList != null)
            {
                bool first = true;
                foreach (var it in c.itemList)
                {
                    if (it == null) continue;
                    if (!first) sb.Append(',');
                    first = false;
                    AppendItem(sb, it, depth);
                }
            }
            sb.Append(']');
        }

        private void AppendItem(StringBuilder sb, Item it, int depth)
        {
            sb.Append("{\"shortname\":").Append(JStr(it.info != null ? it.info.shortname : "?"));
            sb.Append(",\"name\":").Append(JStr(DisplayName(it)));
            sb.Append(",\"amount\":").Append(it.amount);
            sb.Append(",\"slot\":").Append(it.position);
            if (it.info != null)
                sb.Append(",\"cat\":").Append(JStr(it.info.category.ToString()));
            if (it.maxCondition > 0f)
                sb.Append(",\"cond\":").Append((int)Math.Round(100.0 * it.condition / it.maxCondition));
            if (it.contents != null && it.contents.itemList != null && it.contents.itemList.Count > 0 && depth < 3)
            {
                sb.Append(",\"contents\":");
                AppendContainer(sb, it.contents, depth + 1);
            }
            sb.Append('}');
        }

        private static string DisplayName(Item it)
        {
            try
            {
                if (it.info != null && it.info.displayName != null)
                {
                    string n = it.info.displayName.english;
                    if (!string.IsNullOrEmpty(n)) return n;
                }
            }
            catch { }
            return it.info != null ? it.info.shortname : "?";
        }

        private static string JStr(string s)
        {
            if (string.IsNullOrEmpty(s)) return "\"\"";
            var sb = new StringBuilder(s.Length + 2);
            sb.Append('"');
            foreach (char c in s)
            {
                switch (c)
                {
                    case '"': sb.Append("\\\""); break;
                    case '\\': sb.Append("\\\\"); break;
                    case '\n': sb.Append("\\n"); break;
                    case '\r': sb.Append("\\r"); break;
                    case '\t': sb.Append("\\t"); break;
                    default:
                        if (c < 0x20) sb.Append("\\u").Append(((int)c).ToString("x4"));
                        else sb.Append(c);
                        break;
                }
            }
            sb.Append('"');
            return sb.ToString();
        }
    }
}
