// SPDX-License-Identifier: (GPL-2.0-only OR MIT)
/*
 * dctwin-kmsg: the kernel half of dc-twin's in-host agent (internal/inhost, D50).
 *
 * A line written to /proc/dctwin-kmsg is printk'd from kernel context, so the record it leaves in
 * the kernel log has facility kern, as a driver's printk has; a line written to /dev/kmsg from
 * userspace is given facility user by the kernel ("to be able to reliably distinguish
 * kernel-generated messages from userspace-injected ones", printk.c devkmsg_write), and journald
 * then reads a driver line's "NVRM:" as a syslog identifier. The twin uses it to put a fault
 * catalogue's kernelLog lines (an NVIDIA driver's Xid lines) in an os: twin guest's kernel log.
 *
 * Each write is one line, "<N>text" with N the syslog level 0-7 (default 4, KERN_WARNING, at which
 * the NVIDIA driver prints its Xid lines), printk'd as it is: no prefix of the module's own. Only
 * root may write. The file holds no state; nothing reads it.
 */
#include <linux/module.h>
#include <linux/kernel.h>
#include <linux/printk.h>
#include <linux/proc_fs.h>
#include <linux/uaccess.h>

#define DCTWIN_KMSG_MAX 904 /* "<N>", then the in-host agent's 900-byte line (inhost.KmsgWriteLine) */

static ssize_t dctwin_kmsg_write(struct file *file, const char __user *ubuf, size_t len, loff_t *off)
{
	char buf[DCTWIN_KMSG_MAX + 1];
	size_t n = min_t(size_t, len, DCTWIN_KMSG_MAX);
	const char *p = buf;
	int level = 4;

	if (copy_from_user(buf, ubuf, n))
		return -EFAULT;
	buf[n] = '\0';
	while (n > 0 && (buf[n - 1] == '\n' || buf[n - 1] == '\r'))
		buf[--n] = '\0';
	if (n >= 3 && buf[0] == '<' && buf[1] >= '0' && buf[1] <= '7' && buf[2] == '>') {
		level = buf[1] - '0';
		p = buf + 3;
	}
	if (*p == '\0')
		return len;
	switch (level) {
	case 0: printk(KERN_EMERG "%s\n", p); break;
	case 1: printk(KERN_ALERT "%s\n", p); break;
	case 2: printk(KERN_CRIT "%s\n", p); break;
	case 3: printk(KERN_ERR "%s\n", p); break;
	case 4: printk(KERN_WARNING "%s\n", p); break;
	case 5: printk(KERN_NOTICE "%s\n", p); break;
	case 6: printk(KERN_INFO "%s\n", p); break;
	default: printk(KERN_DEBUG "%s\n", p); break;
	}
	return len;
}

static const struct proc_ops dctwin_kmsg_ops = {
	.proc_write = dctwin_kmsg_write,
};

static struct proc_dir_entry *dctwin_kmsg_entry;

static int __init dctwin_kmsg_init(void)
{
	dctwin_kmsg_entry = proc_create("dctwin-kmsg", 0200, NULL, &dctwin_kmsg_ops);
	return dctwin_kmsg_entry ? 0 : -ENOMEM;
}

static void __exit dctwin_kmsg_exit(void)
{
	proc_remove(dctwin_kmsg_entry);
}

module_init(dctwin_kmsg_init);
module_exit(dctwin_kmsg_exit);
MODULE_LICENSE("Dual MIT/GPL");
MODULE_AUTHOR("dc-twin");
MODULE_DESCRIPTION("dc-twin in-host agent: printk a line written to /proc/dctwin-kmsg");
