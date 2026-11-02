import unittest

from mulga_design_check import filled


class FilledTest(unittest.TestCase):
    def test_accepts_either_form(self):
        self.assertTrue(filled("## Mulga design\nUpdated: mulga#275"))
        self.assertTrue(filled("## Mulga design\n\nnot needed: internal refactor\n## Next"))

    def test_rejects_unanswered(self):
        self.assertFalse(filled(""))
        self.assertFalse(filled("## Mulga design\n<!-- Updated: x -->"))
        self.assertFalse(filled("## Mulga design\nUpdated: <mulga PR or bead id>"))
        self.assertFalse(filled("## Mulga design\nUpdated:"))

    def test_scoped_to_the_section(self):
        self.assertFalse(filled("Updated: mulga#1"))
        self.assertFalse(filled("## Mulga design\n## Other\nUpdated: mulga#1"))


if __name__ == "__main__":
    unittest.main()
